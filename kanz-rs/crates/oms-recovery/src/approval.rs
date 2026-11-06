//! Revalidate persisted fee evidence; never originate an approval or infer one
//! from a flag. SQL stores the canonical proposal/approval under the case.
use crate::Error;
use kanz_oms_store::{
    fee_revision, fee_subject, identity,
    recovery_repository::{Case, FeeEvidence, HISTORY_BYTES, MAX_FILLS},
};
use kanz_order_core::{
    proto::order::v1::{
        ExecutionFeeApproval, ExecutionFeeChange, ExecutionFeeCorrectionApproved,
        ExecutionFeeCorrectionProposed, Fill,
    },
    Canonical,
};
use kanz_proto_wire::{Budget, Schema};
use prost::Message;
use sha2::{Digest, Sha256};
use std::collections::BTreeMap;

fn stamp(t: Option<&prost_types::Timestamp>) -> Result<i128, Error> {
    let t = t.ok_or(Error::FeeRevision)?;
    if !(-62135596800..=253402300799).contains(&t.seconds) || !(0..1_000_000_000).contains(&t.nanos)
    {
        return Err(Error::FeeRevision);
    }
    Ok(i128::from(t.seconds) * 1_000_000_000 + i128::from(t.nanos))
}
fn without_digest(proposal: &Canonical<ExecutionFeeCorrectionProposed>) -> Result<Vec<u8>, Error> {
    let number = ExecutionFeeCorrectionProposed::descriptor()?
        .get_field_by_name("digest")
        .ok_or(Error::FeeRevision)?
        .number();
    let mut pos = 0;
    let mut out = Vec::new();
    while pos < proposal.wire().len() {
        let start = pos;
        let (n, w) = kanz_proto_wire::key(proposal.wire(), &mut pos)?;
        kanz_proto_wire::skip(proposal.wire(), &mut pos, n, w)?;
        if !(n == number && w == 2) {
            out.extend_from_slice(&proposal.wire()[start..pos]);
        }
    }
    Ok(out)
}
fn hash(tenant: &str, raw: &[u8]) -> String {
    let mut h = Sha256::new();
    for part in [
        b"EXECUTION_FEE_CORRECTION".as_slice(),
        tenant.as_bytes(),
        raw,
    ] {
        h.update((part.len() as u64).to_be_bytes());
        h.update(part);
    }
    format!("{:x}", h.finalize())
}
pub(crate) struct ApprovedChanges {
    pub by_execution: BTreeMap<String, usize>,
    source: Option<Canonical<ExecutionFeeCorrectionApproved>>,
    case_id: String,
    pub baselines: Vec<(String, Canonical<Fill>)>,
}
pub(crate) fn approved(
    tenant: &str,
    case: &Case,
    history_digest: &str,
    evidence: Option<FeeEvidence>,
) -> Result<ApprovedChanges, Error> {
    let Some(evidence) = evidence else {
        return Ok(ApprovedChanges {
            by_execution: BTreeMap::new(),
            baselines: Vec::new(),
            source: None,
            case_id: case.id.clone(),
        });
    };
    let proposal = Canonical::<ExecutionFeeCorrectionProposed>::decode_with_budget(
        &evidence.proposal,
        Budget::RecoveryHistory,
    )?;
    let approval = Canonical::<ExecutionFeeCorrectionApproved>::decode(&evidence.approval)?;
    let (p, a) = (proposal.value(), approval.value());
    let actual = hash(tenant, &without_digest(&proposal)?);
    let proposed = stamp(p.proposed_at.as_ref())?;
    let expiry = stamp(p.expires_at.as_ref())?;
    let approved = stamp(a.approved_at.as_ref())?;
    // Go time.Sub saturates to int64 duration before Propose adds the TTL.
    // Retain that caller-clock behavior rather than invent a DB-clock policy.
    let ttl = (expiry - proposed).min(i128::from(i64::MAX));
    let zero = -62135596800i128 * 1_000_000_000;
    if tenant.is_empty()
        || actual != evidence.stored_digest
        || p.digest != actual
        || p.case_id != case.id
        || p.case_id.trim().is_empty()
        || p.order_id.trim().is_empty()
        || fee_subject::normalize(&p.proposer).is_empty()
        || p.digest.trim().is_empty()
        || proposed == zero
        || ttl <= 0
        || proposed + ttl == zero
        || fee_subject::normalize(&a.approver).is_empty()
        || fee_subject::normalize(&a.approver) == fee_subject::normalize(&p.proposer)
        || a.digest != p.digest
        || approved >= proposed + ttl
        || a.proposer != p.proposer
        || a.order_id != case.order_id
        || a.case_id != case.id
        || a.portfolio_id != p.portfolio_id
        || p.history_digest != history_digest
        || p.mapping_version != case.mapping_version
        || p.changes.len() > MAX_FILLS
    {
        return Err(Error::FeeRevision);
    }
    let number = ExecutionFeeCorrectionProposed::descriptor()?
        .get_field_by_name("changes")
        .ok_or(Error::FeeRevision)?
        .number();
    let mut pos = 0;
    let mut result = BTreeMap::new();
    let mut baselines = Vec::new();
    while pos < proposal.wire().len() {
        let (n, w) = kanz_proto_wire::key(proposal.wire(), &mut pos)?;
        if n != number || w != 2 {
            kanz_proto_wire::skip(proposal.wire(), &mut pos, n, w)?;
            continue;
        }
        let change = Canonical::<ExecutionFeeChange>::decode_with_budget(
            kanz_proto_wire::payload(proposal.wire(), &mut pos)?,
            Budget::RecoveryHistory,
        )?;
        let previous = Canonical::<Fill>::decode_with_budget(
            &change.message_wire("previous")?.ok_or(Error::FeeRevision)?,
            Budget::RecoveryHistory,
        )?;
        let revised = Canonical::<Fill>::decode_with_budget(
            &change.message_wire("revised")?.ok_or(Error::FeeRevision)?,
            Budget::RecoveryHistory,
        )?;
        let key = identity::execution_key(previous.value());
        if !identity::same_except_fee(previous.value(), revised.value()) {
            return Err(Error::FeeRevision);
        }
        // Validate every repeated change before last-value map selection.
        fee_revision::economic_digest(previous.value())?;
        // Existing Go map behavior uses the final repeated change for an identity.
        result.insert(key.clone(), baselines.len());
        baselines.push((key, previous));
    }
    Ok(ApprovedChanges {
        by_execution: result,
        baselines,
        source: Some(approval),
        case_id: case.id.clone(),
    })
}

impl ApprovedChanges {
    /// Expand one approval at a time; a shared unknown timestamp or actor is
    /// never copied into a retained vector once per historical execution.
    pub fn for_execution(
        &self,
        key: &str,
    ) -> Result<Option<Canonical<ExecutionFeeApproval>>, Error> {
        let Some(index) = self.by_execution.get(key) else {
            return Ok(None);
        };
        let previous = &self.baselines[*index].1;
        let source = self.source.as_ref().ok_or(Error::FeeRevision)?;
        let a = source.value();
        let value = ExecutionFeeApproval {
            proposal_id: self.case_id.clone(),
            digest: a.digest.clone(),
            proposer: a.proposer.clone(),
            approver: a.approver.clone(),
            previous_execution_digest: fee_revision::economic_digest(previous.value())?,
            ..Default::default()
        };
        let mut raw = value.encode_to_vec();
        let desc = ExecutionFeeApproval::descriptor()?;
        for (name, bytes) in [
            (
                "approved_at",
                source
                    .message_wire("approved_at")?
                    .ok_or(Error::FeeRevision)?,
            ),
            (
                "previous_fee",
                previous.message_wire("fee")?.ok_or(Error::FeeRevision)?,
            ),
        ] {
            if raw
                .len()
                .checked_add(bytes.len())
                .and_then(|n| n.checked_add(10))
                .is_none_or(|n| n > HISTORY_BYTES)
            {
                return Err(Error::Capacity);
            }
            kanz_proto_wire::put_message(
                desc.get_field_by_name(name)
                    .ok_or(Error::FeeRevision)?
                    .number(),
                &bytes,
                &mut raw,
            );
        }

        Ok(Some(Canonical::decode_with_budget(
            &raw,
            Budget::RecoveryHistory,
        )?))
    }
}

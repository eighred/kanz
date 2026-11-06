//! Revalidate persisted fee evidence; never originate an approval or infer one
//! from a flag. SQL stores the canonical proposal/approval under the case.
use crate::Error;
use kanz_oms_store::{
    fee_revision, fee_subject, identity,
    recovery_repository::{Case, FeeEvidence, MAX_FILLS},
};
use kanz_order_core::{
    proto::order::v1::{
        ExecutionFeeChange, ExecutionFeeCorrectionApproved, ExecutionFeeCorrectionProposed, Fill,
    },
    Canonical,
};
use kanz_proto_wire::{Budget, Schema};
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
    source: Option<std::sync::Arc<Canonical<ExecutionFeeCorrectionApproved>>>,
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
        source: Some(std::sync::Arc::new(approval)),
    })
}

impl ApprovedChanges {
    pub fn terms(
        &self,
        key: &str,
    ) -> Result<Option<kanz_oms_store::recovery_wire::FeeTerms>, Error> {
        let Some(index) = self.by_execution.get(key) else {
            return Ok(None);
        };
        let source = self.source.as_ref().ok_or(Error::FeeRevision)?;
        Ok(Some(kanz_oms_store::recovery_wire::FeeTerms::new(
            std::sync::Arc::clone(source),
            &self.baselines[*index].1,
        )?))
    }
}

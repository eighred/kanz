//! Revalidate persisted fee evidence; never originate an approval or infer one
//! from a flag. SQL stores the canonical proposal/approval under the case.
use crate::{fee_control, Error};
use kanz_oms_store::{
    fee_revision, identity,
    recovery_repository::{Case, FeeEvidence, MAX_FILLS},
};
use kanz_order_core::{
    proto::order::v1::{
        ExecutionFeeChange, ExecutionFeeCorrectionApproved, ExecutionFeeCorrectionProposed, Fill,
    },
    Canonical,
};
use kanz_proto_wire::{Budget, Schema};
use std::collections::BTreeMap;

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
    let actual = fee_control::digest(tenant, &proposal)?;
    if tenant.is_empty()
        || actual != evidence.stored_digest
        || p.digest != actual
        || p.case_id != case.id
    {
        return Err(Error::FeeRevision);
    }
    // The same Go control validates fresh commands and persisted signatures.
    // Observation refuses these errors without durably acknowledging a block.
    let control = fee_control::Control::new(p)?;
    let approved = fee_control::stamp(a.approved_at.as_ref())?;
    if a.proposer != p.proposer
        || a.order_id != case.order_id
        || a.case_id != case.id
        || a.portfolio_id != p.portfolio_id
        || p.history_digest != history_digest
        || p.mapping_version != case.mapping_version
        || p.changes.len() > MAX_FILLS
    {
        return Err(Error::FeeRevision);
    }
    control
        .approve(&a.approver, &a.digest, approved)
        .map_err(|_| Error::ApprovalRefused)?;
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

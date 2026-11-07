//! Existing delegated-human fee commands. No caller-supplied revised fill,
//! approval flag, venue operation or new admission rule enters this owner.
use crate::{
    fee_control::{self, Control, Refusal},
    handlers, history, observation, preparation, Error,
};
use kanz_oms_store::{
    self as store, fee_commands as repository, fee_subject, identity,
    recovery_repository::{self as recovery, Case},
};
use kanz_order_core::{
    proto::{
        envelope::v1::Envelope,
        order::v1::{
            ApproveExecutionFeeCorrection, ExecutionFeeChange, ExecutionFeeCorrectionApproved,
            ExecutionFeeCorrectionProposed, OrderState, ProposeExecutionFeeCorrection,
        },
    },
    Canonical,
};
use kanz_proto_wire::{Budget, Schema};
use prost::Message;
use std::collections::BTreeMap;
use tokio_postgres::Client;

const PROPOSE: &str = "order.order.propose_fee_correction";
const APPROVE: &str = "order.order.approve_fee_correction";

pub(crate) fn envelope(tenant: &str, raw: &[u8]) -> Result<Canonical<Envelope>, Error> {
    let env = Canonical::<Envelope>::decode(raw)?;
    if tenant.is_empty()
        || env.value().tenant_id != tenant
        || env.value().event_class != 2
        || !matches!(env.value().event_type.as_str(), PROPOSE | APPROVE)
    {
        return Err(Error::InvalidInput);
    }
    // Like the Go handler, this consumes already authenticated Consumer input.
    // No public service/transport is activated by exposing this typed owner.
    Ok(env)
}
fn repository_error(error: store::Error) -> Error {
    match error {
        store::Error::IdentityConflict => Error::RecoveryEvidenceConflict,
        other => other.into(),
    }
}
fn refusal(error: Refusal) -> Error {
    match error {
        Refusal::Malformed => Error::ApprovalRefused,
        Refusal::SelfApproval => Error::SelfApproval,
        Refusal::PayloadChanged => Error::PayloadChanged,
        Refusal::Expired => Error::ApprovalExpired,
    }
}
async fn proposal(
    client: &mut Client,
    tenant: &str,
    case: &str,
) -> Result<Option<Canonical<ExecutionFeeCorrectionProposed>>, Error> {
    let Some(stored) = repository::proposal(client, tenant, case).await? else {
        return Ok(None);
    };
    let p = Canonical::<ExecutionFeeCorrectionProposed>::decode(&stored.raw)?;
    let actual = fee_control::digest(tenant, &p)?;
    if actual != stored.digest || p.value().digest != actual || p.value().case_id != case {
        return Err(Error::FeeRevision);
    }
    Ok(Some(p))
}
fn message_size(field: u32, bytes: usize) -> Result<usize, Error> {
    fn width(n: u64) -> usize {
        (64 - n.leading_zeros() as usize).max(1).div_ceil(7)
    }
    bytes
        .checked_add(width(u64::from(field) << 3 | 2))
        .and_then(|n| n.checked_add(width(bytes as u64)))
        .ok_or(Error::Capacity)
}
async fn propose(
    client: &mut Client,
    tenant: &str,
    env: &Envelope,
    c: &Case,
    actor: &str,
    reason: &str,
    now: prost_types::Timestamp,
) -> Result<(), Error> {
    if !recovery::text(actor, 256) || !recovery::text(reason, 2048) || now.seconds <= 0 {
        return Err(Error::FeeRevision);
    }
    if let Some(old) = proposal(client, tenant, &c.id).await? {
        let old = old.value();
        if old.proposer != actor || old.reason != reason || old.order_id != c.order_id {
            return Err(Error::RecoveryEvidenceConflict);
        }
        return Ok(());
    }
    if c.checkpoint != 0 || !matches!(c.status.as_str(), "blocked" | "investigating") {
        return Err(Error::FeeRevision);
    }
    let (frozen, history_digest) = recovery::history(client, tenant, &c.id)
        .await
        .map_err(repository_error)?;
    let loaded = store::load(client, tenant, &c.order_id).await?;
    let state = Canonical::<OrderState>::decode(&loaded.state)?;
    let observed = history::frozen_fills(&frozen)?;
    let observed: BTreeMap<_, _> = observed
        .iter()
        .map(|f| (identity::execution_key(f.value()), f))
        .collect();
    let p = ExecutionFeeCorrectionProposed {
        case_id: c.id.clone(),
        order_id: c.order_id.clone(),
        portfolio_id: state.value().portfolio_id.clone(),
        mapping_version: c.mapping_version.clone(),
        history_digest,
        proposer: actor.into(),
        reason: reason.into(),
        proposed_at: Some(now),
        expires_at: Some(prost_types::Timestamp {
            seconds: now
                .seconds
                .checked_add(fee_control::DEFAULT_TTL_SECONDS)
                .ok_or(Error::FeeRevision)?,
            nanos: now.nanos,
        }),
        ..Default::default()
    };
    let mut used = ExecutionFeeCorrectionProposed {
        digest: "0".repeat(64),
        ..p.clone()
    }
    .encoded_len();
    let field = ExecutionFeeCorrectionProposed::descriptor()?
        .get_field_by_name("changes")
        .ok_or(Error::InvalidInput)?
        .number();
    let mut changes = BTreeMap::new();
    let descriptor = ExecutionFeeChange::descriptor()?;
    let previous_field = descriptor
        .get_field_by_name("previous")
        .ok_or(Error::InvalidInput)?
        .number();
    let revised_field = descriptor
        .get_field_by_name("revised")
        .ok_or(Error::InvalidInput)?
        .number();
    preparation::posted_with(client, tenant, &state, &[], None, |key, previous| {
        let revised = observed.get(key).ok_or(Error::FeeRevision)?;
        if !identity::same_except_fee(previous.value(), revised.value()) {
            return Err(Error::FeeRevision);
        }
        if identity::same_execution(previous.value(), revised.value()) {
            return Ok(());
        }
        // Preflight before retaining changed raw fills. Unknown nested fields
        // are part of the review digest and must never be prost-remarshalled away.
        let previous_raw = previous.journal_bytes()?;
        let change_bytes = message_size(previous_field, previous_raw.len())?
            .checked_add(message_size(revised_field, revised.wire().len())?)
            .ok_or(Error::Capacity)?;
        let cost = message_size(field, change_bytes)?;
        used = used.checked_add(cost).ok_or(Error::Capacity)?;
        if used > repository::PROPOSAL_BYTES {
            return Err(Error::Capacity);
        }
        let mut raw = Vec::new();
        kanz_proto_wire::append_message(previous_field, &previous_raw, &mut raw, Budget::Standard)?;
        kanz_proto_wire::append_message(revised_field, revised.wire(), &mut raw, Budget::Standard)?;
        changes.insert(key.to_owned(), raw);
        Ok(())
    })
    .await
    .map_err(|e| match e {
        preparation::PostedError::Snapshot(e) | preparation::PostedError::Evidence(e) => e,
    })?;
    if changes.is_empty() {
        return Err(Error::FeeRevision);
    }
    let mut raw = p.encode_to_vec();
    for change in changes.values() {
        kanz_proto_wire::append_message(field, change, &mut raw, Budget::Standard)?;
    }
    let unsigned = Canonical::<ExecutionFeeCorrectionProposed>::decode(&raw)?;
    let digest = fee_control::digest(tenant, &unsigned)?;
    raw.extend(
        ExecutionFeeCorrectionProposed {
            digest,
            ..Default::default()
        }
        .encode_to_vec(),
    );
    let proposed = Canonical::<ExecutionFeeCorrectionProposed>::decode(&raw)?;
    Control::new(proposed.value())?;
    if proposed.wire().len() > repository::PROPOSAL_BYTES {
        return Err(Error::Capacity);
    }
    let fact = handlers::fact(
        env,
        &c.order_id,
        "order.order.fee_correction_proposed",
        "order.v1.ExecutionFeeCorrectionProposed:1",
        proposed.wire().to_vec(),
        now,
    )?;
    repository::save_proposal(
        client,
        tenant,
        state.wire(),
        loaded.version,
        &proposed,
        &fact,
    )
    .await
    .map_err(repository_error)
}
struct ApprovalInput<'a> {
    actor: &'a str,
    digest: &'a str,
}
async fn approve(
    client: &mut Client,
    tenant: &str,
    env: &Envelope,
    c: &Case,
    state: &Canonical<OrderState>,
    input: ApprovalInput<'_>,
    now: prost_types::Timestamp,
) -> Result<(), Error> {
    let ApprovalInput { actor, digest } = input;
    let p = proposal(client, tenant, &c.id)
        .await?
        .ok_or(Error::FeeRevision)?;
    let p = p.value();
    let control = Control::new(p)?;
    let at = fee_control::stamp(Some(&now))?;
    if p.order_id != c.order_id
        || !recovery::text(fee_subject::trim(actor), 256)
        || actor.len() > 256
        || at < control.created
    {
        return Err(Error::FeeRevision);
    }
    if let Some(saved) = repository::approval(client, tenant, &c.id).await? {
        let old = Canonical::<ExecutionFeeCorrectionApproved>::decode(&saved)?;
        let old = old.value();
        if old.digest != digest
            || fee_subject::normalize(&old.approver) != fee_subject::normalize(actor)
        {
            return Err(Error::RecoveryEvidenceConflict);
        }
        let signed = fee_control::stamp(old.approved_at.as_ref())
            .map_err(|_| Error::RecoveryEvidenceConflict)?;
        return control
            .approve(&old.approver, &old.digest, signed)
            .map_err(refusal);
    }
    control.approve(actor, digest, at).map_err(refusal)?;
    let approved = Canonical::from_value(ExecutionFeeCorrectionApproved {
        case_id: c.id.clone(),
        order_id: c.order_id.clone(),
        portfolio_id: p.portfolio_id.clone(),
        digest: digest.into(),
        proposer: p.proposer.clone(),
        approver: actor.into(),
        approved_at: Some(now),
    })?;
    let fact = handlers::fact(
        env,
        &c.order_id,
        "order.order.fee_correction_approved",
        "order.v1.ExecutionFeeCorrectionApproved:1",
        approved.wire().to_vec(),
        now,
    )?;
    repository::save_approval(client, tenant, c, state.wire(), &approved, &fact)
        .await
        .map_err(repository_error)
}

pub(crate) async fn handle(
    client: &mut Client,
    tenant: &str,
    env: &Envelope,
    payload: &[u8],
    now: prost_types::Timestamp,
) -> Result<String, Error> {
    if payload.is_empty() || payload.len() > 65536 {
        return Err(Error::InvalidInput);
    }
    let (metadata, order, case, argument) = match env.event_type.as_str() {
        PROPOSE => {
            let c = Canonical::<ProposeExecutionFeeCorrection>::decode(payload)?;
            let c = c.value();
            (
                c.metadata.clone(),
                c.order_id.clone(),
                c.case_id.clone(),
                c.reason.clone(),
            )
        }
        APPROVE => {
            let c = Canonical::<ApproveExecutionFeeCorrection>::decode(payload)?;
            let c = c.value();
            (
                c.metadata.clone(),
                c.order_id.clone(),
                c.case_id.clone(),
                c.digest.clone(),
            )
        }
        _ => return Err(Error::InvalidInput),
    };
    let m = metadata.ok_or(Error::InvalidInput)?;
    if !recovery::text(&order, 256)
        || !recovery::text(&case, 256)
        || m.target_id != order
        || !m
            .issuer
            .strip_prefix("user:")
            .is_some_and(|subject| recovery::text(subject, 251))
    {
        return Err(Error::InvalidInput);
    }
    let loaded = store::load(client, tenant, &order).await?;
    let state = Canonical::<OrderState>::decode(&loaded.state)?;
    if state.value().portfolio_id.is_empty()
        || !m.principal_portfolios.contains(&state.value().portfolio_id)
    {
        return Err(Error::InvalidInput);
    }
    let mut c = recovery::load_case(client, tenant, &case).await?;
    if c.order_id != order {
        return Err(Error::RecoveryEvidenceConflict);
    }
    if env.event_type == PROPOSE {
        propose(client, tenant, env, &c, &m.issuer, &argument, now).await?;
    } else {
        let result = approve(
            client,
            tenant,
            env,
            &c,
            &state,
            ApprovalInput {
                actor: &m.issuer,
                digest: &argument,
            },
            now,
        )
        .await;
        if c.checkpoint == 0 && c.status != "corrected" {
            let reason = match result.as_ref().err() {
                Some(Error::SelfApproval) => {
                    Some("fee approval refused: proposer and approver must be different people")
                }
                Some(Error::ApprovalExpired) => {
                    Some("fee approval refused: proposal expired; start a new investigation")
                }
                Some(Error::PayloadChanged) => {
                    Some("fee approval refused: digest does not match the proposed changes")
                }
                _ => None,
            };
            if let Some(reason) = reason {
                return observation::blocked(client, tenant, env, &c, state.value(), reason, now)
                    .await;
            }
        }
        result?;
        c = recovery::load_case(client, tenant, &case).await?;
        if c.status == "investigating" && c.checkpoint == 0 {
            match observation::prepared(client, tenant, env, &c, now).await {
                Ok(()) => {}
                Err(preparation::ProjectionError::Error(
                    Error::FeeRevision | Error::EvidenceConflict | Error::PostedHistoryUnknown,
                )) => {
                    return observation::blocked(client, tenant, env, &c, state.value(), "approved fee correction no longer matches the booked execution; a new investigation is required", now).await;
                }
                Err(preparation::ProjectionError::Block) => {
                    observation::blocked(client, tenant, env, &c, state.value(), "verified execution cannot be represented in the order aggregate; operator investigation required", now).await?;
                    return Err(Error::RecoveryBlocked);
                }
                Err(preparation::ProjectionError::Error(error)) => return Err(error),
            }
        }
    }
    Ok(order)
}

//! Durable handler phases. The coordinator closes SQL sessions before relay
//! Flush; a successful blocked-status commit is not financial recovery success.
use crate::{history, Error};
use kanz_oms_store::diagnostics::{measure, Stage};
use kanz_oms_store::{
    self as store, fee_revision,
    fee_wire::FillWire,
    identity,
    recovery_ack::{self, Book},
    recovery_repository::{self as repository, Case},
    Record,
};
use kanz_order_core::{
    proto::{
        envelope::v1::Envelope,
        order::v1::{
            ExecutionRecovered, ExecutionRecoveryApplied, ExecutionRecoveryRecorded, OrderState,
        },
    },
    Canonical,
};
use prost::Message;
use std::time::SystemTime;
use tokio_postgres::Client;

pub(crate) fn envelope(tenant: &str, raw: &[u8]) -> Result<Canonical<Envelope>, Error> {
    let env = Canonical::<Envelope>::decode(raw)?;
    if tenant.is_empty() || env.value().tenant_id != tenant || env.value().event_class != 1 {
        return Err(Error::InvalidInput);
    }
    // Direct Go handlers require FACT, exact tenant and their selected subject;
    // full authenticated Consumer transport validation remains a separate owner.
    Ok(env)
}
pub(crate) fn status(
    env: &Envelope,
    c: &Case,
    st: &OrderState,
    now: prost_types::Timestamp,
) -> Result<Record, Error> {
    if c.checkpoint < 0
        || c.checkpoint as usize > repository::MAX_FILLS
        || !(-62135596800..=253402300799).contains(&now.seconds)
        || !(0..1_000_000_000).contains(&now.nanos)
    {
        return Err(Error::InvalidInput);
    }
    let event = ExecutionRecoveryRecorded {
        case_id: c.id.clone(),
        order_id: c.order_id.clone(),
        portfolio_id: st.portfolio_id.clone(),
        venue: st.venue.clone(),
        venue_account_id: st.venue_account_id.clone(),
        mapping_version: c.mapping_version.clone(),
        source_cursor: c.source_cursor.clone(),
        payload_digest: c.digest.clone(),
        status: c.status.clone(),
        reason: c.reason.clone(),
        checkpoint: c.checkpoint as u32,
        recorded_at: Some(now),
    };
    fact(
        env,
        &c.order_id,
        "order.order.recovery_recorded",
        "order.v1.ExecutionRecoveryRecorded:1",
        event.encode_to_vec(),
        now,
    )
}
pub(crate) fn fact(
    env: &Envelope,
    order: &str,
    subject: &str,
    schema: &str,
    payload: Vec<u8>,
    now: prost_types::Timestamp,
) -> Result<Record, Error> {
    let at = SystemTime::try_from(now).map_err(|_| Error::InvalidInput)?;
    let metadata = [
        env.tenant_id.as_str(),
        order,
        env.correlation_id.as_str(),
        env.event_id.as_str(),
        env.trace_context.as_str(),
    ]
    .iter()
    .try_fold(payload.len(), |n, s| n.checked_add(s.len()))
    .ok_or(Error::Capacity)?;
    if metadata > store::MAX_BYTES {
        return Err(Error::Capacity);
    }
    Ok(Record {
        tenant: env.tenant_id.clone(),
        partition_key: order.into(),
        subject: subject.into(),
        event_type: subject.into(),
        event_class: 1,
        schema_version: 1,
        domain: "order".into(),
        payload_schema_ref: schema.into(),
        event_time: at,
        correlation_id: env.correlation_id.clone(),
        causation_id: env.event_id.clone(),
        trace_context: env.trace_context.clone(),
        payload,
    })
}
pub(crate) async fn ack(
    client: &mut Client,
    tenant: &str,
    env: &Envelope,
    payload: &[u8],
    now: prost_types::Timestamp,
) -> Result<String, Error> {
    if payload.len() > 4096 {
        return Err(Error::Capacity);
    }
    let book = match env.event_type.as_str() {
        "order.order.recovery_position_applied" => Book::Position,
        "order.order.recovery_ledger_applied" => Book::Ledger,
        _ => return Err(Error::InvalidInput),
    };
    let ack = Canonical::<ExecutionRecoveryApplied>::decode(payload)?;
    let ack = ack.value();
    let (mut c, loaded) =
        repository::load_ack_context(client, tenant, &ack.case_id, &ack.order_id).await?;
    let state = Canonical::<OrderState>::decode(&loaded.state)?;
    c.status = "corrected".into();
    c.reason.clear();
    let record = status(env, &c, state.value(), now)?;
    measure(
        Stage::DurableAck,
        recovery_ack::acknowledge_recovery(client, tenant, ack, book, &record),
    )
    .await?;
    Ok(ack.order_id.clone())
}
pub(crate) async fn parked(
    client: &mut Client,
    tenant: &str,
    env: &Envelope,
    payload: &[u8],
    now: prost_types::Timestamp,
) -> Result<String, Error> {
    if env.event_type != "order.order.execution_recovered" {
        return Err(Error::InvalidInput);
    }
    let event = Canonical::<ExecutionRecovered>::decode(payload)?;
    history::domain(event.value())?;
    let f = event.value().fill.as_ref().ok_or(Error::InvalidInput)?;
    let st = event.value().state.as_ref().ok_or(Error::InvalidInput)?;
    let p = f.recovery.as_ref().ok_or(Error::InvalidInput)?;
    if st.portfolio_id.is_empty()
        || f.order_id != st.order_id
        || f.venue != st.venue
        || f.venue_account_id != st.venue_account_id
        || f.instrument_id != st.instrument_id
        || f.side != st.side
        || f.fill_id.is_empty()
        || f.venue.is_empty()
        || !kanz_decimal_compat::is_positive(f.quantity.as_ref())
    {
        return Err(Error::InvalidInput);
    }
    let wire = FillWire::decode(&event.message_wire("fill")?.ok_or(Error::InvalidInput)?)?;
    if repository::digest(&wire.without_recovery()?.journal_bytes()?) != p.execution_digest {
        return Err(Error::EvidenceConflict);
    }
    if p.fee_approval.is_some() {
        fee_revision::terms(&wire)?;
    }
    for _ in 0..4 {
        let mut c = repository::load_case(client, tenant, &p.case_id).await?;
        if c.order_id != f.order_id
            || !repository::target_matches(
                client,
                tenant,
                &c.id,
                &identity::execution_key(f),
                &p.payload_digest,
                &p.execution_digest,
            )
            .await?
        {
            return Err(Error::EvidenceConflict);
        }
        if c.status == "corrected" || c.status == "blocked" {
            return Ok(c.order_id);
        }
        let loaded = store::load(client, tenant, &c.order_id).await?;
        let state = Canonical::<OrderState>::decode(&loaded.state)?;
        let reason =
            "a recovered execution delivery was parked; book acknowledgements remain incomplete";
        c.status = "blocked".into();
        c.reason = reason.into();
        let record = status(env, &c, state.value(), now)?;
        match repository::block(client, tenant, &c, state.value(), reason, &record).await {
            Ok(()) => return Ok(c.order_id),
            Err(store::Error::Conflict) => {}
            Err(e) => return Err(e.into()),
        }
    }
    Err(Error::Conflict)
}

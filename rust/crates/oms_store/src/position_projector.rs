//! Go position.Projector.Handle composition; no subscriber or production caller.
//! Delivery authentication/validation remains at the canonical Consumer boundary.
use crate::{
    fee_revision,
    fee_wire::FillWire,
    identity, position,
    relay::{Publisher, Relay, Sessions},
    Error, Record, MAX_BYTES,
};
use kanz_order_core::proto::{
    domain::v1::PositionState,
    envelope::v1::{Envelope, EventClass},
    order::v1::{ExecutionRecovered, ExecutionRecoveryApplied, OrderFilled, OrderPartiallyFilled},
    FILE_DESCRIPTOR_SET,
};
use prost::Message;
use prost_reflect::{DescriptorPool, DynamicMessage};
use sha2::{Digest, Sha256};
use std::{fmt::Write, time::SystemTime};
use tokio_postgres::Client;
const FILLED: &str = "order.order.filled";
const PARTIAL: &str = "order.order.partially_filled";
const RECOVERED: &str = "order.order.execution_recovered";
const ACK: &str = "order.order.recovery_position_applied";
const AGGREGATE: &str = "risk.position.changed";
const VENUE: &str = "risk.position.venue.changed";
pub struct Outcome {
    pub ignored: bool,
    pub sent: usize,
}
fn token(s: &str) -> String {
    if s.is_empty() {
        return "%_EMPTY".into();
    }
    let mut out = String::new();
    for b in s.bytes() {
        if (0x21..=0x7e).contains(&b) && !b".*>%".contains(&b) {
            out.push(char::from(b));
        } else {
            write!(out, "%{b:02X}").expect("String write");
        }
    }
    out
}
// The existing normalizer merges singular duplicate messages while retaining
// unknown groups. Scan with the same bounded wire primitives, not a new codec.
fn nested_fill(raw: &[u8], field: u32) -> Result<Vec<u8>, Error> {
    let mut pos = 0;
    let mut found = None;
    while pos < raw.len() {
        let (n, w) = crate::fee_wire::key(raw, &mut pos)?;
        if n == field && w == 2 {
            if found.is_some() {
                return Err(Error::InvalidInput);
            }
            found = Some(crate::fee_wire::payload(raw, &mut pos)?.to_vec());
        } else {
            crate::fee_wire::skip(raw, &mut pos, n, w)?;
        }
    }
    found.ok_or(Error::InvalidInput)
}
fn domain(msg: &DynamicMessage, depth: usize) -> Result<(), Error> {
    kanz_order_core::decimal_domain(msg, depth).map_err(Error::from)
}

fn time(t: Option<&prost_types::Timestamp>) -> Result<SystemTime, Error> {
    let value = t.copied().unwrap_or_default();
    if !identity::timestamp(&value) {
        return Err(Error::InvalidInput);
    }
    SystemTime::try_from(value).map_err(|_| Error::InvalidInput)
}
#[allow(clippy::too_many_arguments)]
fn record(
    env: &Envelope,
    key: &str,
    subject: String,
    event_type: &str,
    schema: &str,
    payload: Vec<u8>,
    at: SystemTime,
    domain: &str,
) -> Result<Record, Error> {
    let stamp = prost_types::Timestamp::from(at);
    if stamp.seconds == -62135596800 && stamp.nanos == 0 {
        return Err(Error::InvalidInput);
    }
    Ok(Record {
        tenant: if env.tenant_id.is_empty() {
            "__system__".into()
        } else {
            env.tenant_id.clone()
        },
        partition_key: key.into(),
        subject,
        event_type: event_type.into(),
        event_class: EventClass::Fact as i32,
        schema_version: 1,
        domain: domain.into(),
        payload_schema_ref: schema.into(),
        event_time: at,
        correlation_id: env.correlation_id.clone(),
        causation_id: env.event_id.clone(),
        trace_context: env.trace_context.clone(),
        payload,
    })
}
fn position_record(
    env: &Envelope,
    key: &str,
    subject: String,
    event_type: &str,
    state: &PositionState,
) -> Result<Record, Error> {
    record(
        env,
        key,
        subject,
        event_type,
        "domain.v1.PositionState:1",
        state.encode_to_vec(),
        time(state.as_of.as_ref())?,
        "risk",
    )
}
fn recovery_ack(
    env: &Envelope,
    key: &str,
    wire: &FillWire,
    now: SystemTime,
) -> Result<Option<Record>, Error> {
    let f = wire.value();
    let Some(p) = f.recovery.as_ref() else {
        return Ok(None);
    };
    if p.payload_digest.len() != 64
        || !p.payload_digest.bytes().all(|b| b.is_ascii_hexdigit())
        || p.case_id.is_empty()
        || p.mapping_version.is_empty()
        || p.source_cursor.is_empty()
        || f.order_id.is_empty()
        || identity::execution_key(f) == f.fill_id
    {
        return Err(Error::InvalidInput);
    }
    let digest = format!(
        "{:x}",
        Sha256::digest(wire.without_recovery()?.journal_bytes()?)
    );
    if digest != p.execution_digest {
        return Err(Error::InvalidInput);
    }
    let at = prost_types::Timestamp::from(now);
    if !identity::timestamp(&at) {
        return Err(Error::InvalidInput);
    }
    let ack = ExecutionRecoveryApplied {
        case_id: p.case_id.clone(),
        order_id: f.order_id.clone(),
        execution_key: identity::execution_key(f),
        payload_digest: p.payload_digest.clone(),
        execution_digest: digest,
        recorded_at: Some(at),
    };
    Ok(Some(record(
        env,
        key,
        ACK.into(),
        ACK,
        "order.v1.ExecutionRecoveryApplied:1",
        ack.encode_to_vec(),
        now,
        "order",
    )?))
}
/// Flush refusal can follow a committed fold. Retry the original delivery;
/// PublicationUnconfirmed and CommitUnconfirmed do not assert rollback.
#[allow(clippy::too_many_arguments)]
pub async fn handle<P: Publisher, S: Sessions>(
    client: &mut Client,
    relay: &mut Relay<P, S>,
    tenant: &str,
    envelope: &[u8],
    payload: &[u8],
    now: SystemTime,
    currency: &str,
) -> Result<Outcome, Error> {
    if envelope.len() > MAX_BYTES || payload.len() > MAX_BYTES - envelope.len() {
        return Err(Error::Capacity);
    }
    let env = Envelope::decode(envelope).map_err(|_| Error::InvalidInput)?;
    // Explicit legacy shared-bucket exception; recovery never uses it.
    if tenant.is_empty() || (tenant != "__system__" && env.tenant_id != tenant) {
        return Err(Error::InvalidInput);
    }
    let (name, fill_field) = match env.event_type.as_str() {
        FILLED => ("order.v1.OrderFilled", 2),
        PARTIAL => ("order.v1.OrderPartiallyFilled", 2),
        RECOVERED => ("order.v1.ExecutionRecovered", 1),
        _ => {
            return Ok(Outcome {
                ignored: true,
                sent: 0,
            })
        }
    };
    let pool = DescriptorPool::decode(FILE_DESCRIPTOR_SET).map_err(|_| Error::InvalidInput)?;
    let desc = pool.get_message_by_name(name).ok_or(Error::InvalidInput)?;
    let normalized = crate::relay_wire::normalize(desc.clone(), payload)?;
    let dynamic =
        DynamicMessage::decode(desc, normalized.as_slice()).map_err(|_| Error::InvalidInput)?;
    domain(&dynamic, 0)?;
    let (fill, state) = match env.event_type.as_str() {
        FILLED => {
            let e = OrderFilled::decode(normalized.as_slice()).map_err(|_| Error::InvalidInput)?;
            (e.fill, e.state)
        }
        PARTIAL => {
            let e = OrderPartiallyFilled::decode(normalized.as_slice())
                .map_err(|_| Error::InvalidInput)?;
            (e.fill, e.state)
        }
        _ => {
            let e = ExecutionRecovered::decode(normalized.as_slice())
                .map_err(|_| Error::InvalidInput)?;
            (e.fill, e.state)
        }
    };
    if env.event_type == RECOVERED
        && (fill.as_ref().and_then(|f| f.recovery.as_ref()).is_none()
            || state.as_ref().is_none_or(|s| s.portfolio_id.is_empty()))
    {
        return Err(Error::InvalidInput);
    }
    let Some(fill) = fill else {
        return Ok(Outcome {
            ignored: true,
            sent: 0,
        });
    };
    let wire = FillWire::decode(&nested_fill(&normalized, fill_field)?)?;
    let portfolio = state
        .as_ref()
        .map(|s| s.portfolio_id.as_str())
        .unwrap_or_default();
    if let Some(p) = fill.recovery.as_ref() {
        if env.event_type != RECOVERED
            || env.event_class != EventClass::Fact as i32
            || env.tenant_id != tenant
        {
            return Err(Error::InvalidInput);
        }
        let st = state.as_ref().ok_or(Error::InvalidInput)?;
        if fill.order_id != st.order_id
            || fill.venue != st.venue
            || fill.venue_account_id != st.venue_account_id
            || fill.instrument_id != st.instrument_id
            || fill.side != st.side
            || p.execution_digest
                != format!(
                    "{:x}",
                    Sha256::digest(wire.without_recovery()?.journal_bytes()?)
                )
        {
            return Err(Error::InvalidInput);
        }
        if p.fee_approval.is_some() {
            fee_revision::terms(&wire)?;
        }
    }
    let key = format!(
        "{AGGREGATE}.{}.{}.{}",
        token(tenant),
        token(portfolio),
        token(&fill.instrument_id)
    );
    let venue = format!(
        "{VENUE}.{}.{}.{}.{}",
        token(tenant),
        token(portfolio),
        token(&fill.venue),
        token(&fill.instrument_id)
    );
    position::apply(
        client,
        tenant,
        portfolio,
        wire.original(),
        time(fill.executed_at.as_ref())?,
        currency,
        |applied| {
            let mut records = vec![
                position_record(&env, &key, key.clone(), AGGREGATE, &applied.aggregate)?,
                position_record(&env, &key, venue, VENUE, &applied.venue)?,
            ];
            if let Some(ack) = recovery_ack(&env, &key, &wire, now)? {
                records.push(ack);
            }
            Ok(records)
        },
    )
    .await?;
    let out = relay.flush(&key).await.map_err(|e| e.error)?;
    if out.stalled.is_some() {
        return Err(Error::PublicationUnconfirmed);
    }
    Ok(Outcome {
        ignored: false,
        sent: out.sent,
    })
}
#[cfg(test)]
mod tests {
    use super::*;
    use prost_reflect::Value;
    #[test]
    fn subject_tokens_are_injective() {
        assert_eq!(token(""), "%_EMPTY");
        assert_eq!(token("VOD.L %*"), "VOD%2EL%20%25%2A");
        assert_ne!(token("."), token("%2E"));
    }
    #[test]
    fn decimal_domain_covers_state_not_only_fill() {
        let pool = DescriptorPool::decode(FILE_DESCRIPTOR_SET).unwrap();
        let mut decimal =
            DynamicMessage::new(pool.get_message_by_name("common.v1.Decimal").unwrap());
        decimal.set_field_by_name("exponent", Value::I32(65));
        let mut state =
            DynamicMessage::new(pool.get_message_by_name("order.v1.OrderState").unwrap());
        state.set_field_by_name("ordered_quantity", Value::Message(decimal));
        let mut event =
            DynamicMessage::new(pool.get_message_by_name("order.v1.OrderFilled").unwrap());
        event.set_field_by_name("state", Value::Message(state));
        assert_eq!(domain(&event, 0), Err(Error::InvalidInput));
    }
    #[test]
    fn nested_fill_does_not_discard_unknown_group() {
        // field2 empty Fill, then unknown field3 group with a scalar child.
        assert_eq!(
            nested_fill(&[0x12, 0, 0x1b, 0x20, 1, 0x1c], 2).unwrap(),
            Vec::<u8>::new()
        );
        assert!(nested_fill(&[0x12, 0, 0x1b], 2).is_err());
    }
}

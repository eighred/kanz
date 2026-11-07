//! Durable tail of Go CommitRecovery, not history preparation or approval admission.
//! Trusted caller supplies the verified complete projection and immutable evidence.
use super::{
    claim, commit, duplicate, enqueue, fee_journal, fee_revision, fee_wire::FillWire, identity,
    journal, message_field, scoped, validate_batch, Error, Record, MAX_BYTES,
};
use kanz_decimal_compat::{in_domain, is_positive};
use kanz_order_core::proto::order::v1::{
    ExecutionRecovered, ExecutionRecoveryRecorded, OrderState,
};
use prost::Message;
use sha2::{Digest, Sha256};
use std::{
    collections::{BTreeMap, BTreeSet},
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio_postgres::Client;

const MAX_FILLS: usize = 8000;
const MAX_BATCH: usize = 4 * MAX_BYTES;
const RECOVERED: &str = "order.order.execution_recovered";
const RECORDED: &str = "order.order.recovery_recorded";
const WAITING: &str =
    "OMS committed execution evidence; awaiting position and ledger acknowledgements";

/// Internal repository input, not a public transport or authorization capability.
/// Go owns CompleteHistory, mapping/approval validation, posted-history completeness,
/// terminal fold and once-rounded complete average before constructing this batch.
pub struct PreparedRecovery {
    pub state: Vec<u8>,
    pub order_version: i64,
    pub case_version: i64,
    pub history_digest: String,
    pub missing: Vec<String>,
    pub posted: Vec<FillWire>,
    /// Exactly one recovered FACT per complete-history execution, then one status FACT.
    pub records: Vec<Record>,
}
pub(super) fn time(t: &prost_types::Timestamp) -> Result<SystemTime, Error> {
    if !identity::timestamp(t) {
        return Err(Error::InvalidInput);
    }
    let base = if t.seconds >= 0 {
        UNIX_EPOCH.checked_add(Duration::from_secs(t.seconds as u64))
    } else {
        UNIX_EPOCH.checked_sub(Duration::from_secs(t.seconds.unsigned_abs()))
    };
    base.and_then(|v| v.checked_add(Duration::from_nanos(t.nanos as u64)))
        .ok_or(Error::InvalidInput)
}
pub(super) fn fact(
    r: &Record,
    tenant: &str,
    order: &str,
    subject: &str,
    schema: &str,
    at: SystemTime,
) -> Result<(), Error> {
    if r.tenant != tenant
        || r.partition_key != order
        || r.subject != subject
        || r.event_type != subject
        || r.event_class != 1
        || r.schema_version != 1
        || r.domain != "order"
        || r.payload_schema_ref != schema
        || r.event_time != at
        || r.payload.len() > MAX_BYTES
    {
        return Err(Error::InvalidInput);
    }
    Ok(())
}
fn projection(batch: &PreparedRecovery) -> Result<OrderState, Error> {
    if batch.records.is_empty()
        || batch.posted.len() > MAX_FILLS
        || batch.missing.len() > MAX_FILLS
        || batch.history_digest.len() != 64
        || !batch.history_digest.bytes().all(|b| b.is_ascii_hexdigit())
    {
        return Err(Error::InvalidInput);
    }
    let extra = batch
        .posted
        .iter()
        .try_fold(batch.history_digest.len(), |n, f| {
            n.checked_add(f.original().len())
        })
        .and_then(|n| {
            batch
                .missing
                .iter()
                .try_fold(n, |n, k| n.checked_add(k.len()))
        })
        .ok_or(Error::Capacity)?;
    let budget = MAX_BATCH.checked_sub(extra).ok_or(Error::Capacity)?;
    validate_batch(&batch.state, &batch.records, MAX_FILLS + 1, budget)
}
/// All durable owners commit together. Failure before confirmed commit rolls back
/// by dropping the transaction; CommitUnconfirmed never implies safe retry.
pub async fn commit_prepared_recovery(
    client: &mut Client,
    tenant: &str,
    batch: &PreparedRecovery,
) -> Result<(), Error> {
    let st = projection(batch)?;
    let status_record = batch.records.last().ok_or(Error::InvalidInput)?;
    let status = ExecutionRecoveryRecorded::decode(status_record.payload.as_slice())
        .map_err(|_| Error::InvalidInput)?;
    let at = time(status.recorded_at.as_ref().ok_or(Error::InvalidInput)?)?;
    fact(
        status_record,
        tenant,
        &st.order_id,
        RECORDED,
        "order.v1.ExecutionRecoveryRecorded:1",
        at,
    )?;
    let count = batch.records.len() - 1;
    if status.case_id.is_empty()
        || status.case_id.len() > 256
        || status.order_id != st.order_id
        || status.portfolio_id != st.portfolio_id
        || status.venue != st.venue
        || status.venue_account_id != st.venue_account_id
        || status.checkpoint as usize != count
        || status.mapping_version.is_empty()
        || status.source_cursor.is_empty()
        || status.payload_digest.is_empty()
        || status.status
            != if count == 0 {
                "corrected"
            } else {
                "investigating"
            }
        || status.reason != if count == 0 { "" } else { WAITING }
    {
        return Err(Error::InvalidInput);
    }
    if ![
        st.ordered_quantity.as_ref(),
        st.filled_quantity.as_ref(),
        st.leaves_quantity.as_ref(),
        st.average_fill_price.as_ref(),
    ]
    .into_iter()
    .all(in_domain)
    {
        return Err(Error::InvalidInput);
    }
    let mut fills = BTreeMap::new();
    for r in &batch.records[..count] {
        fact(
            r,
            tenant,
            &st.order_id,
            RECOVERED,
            "order.v1.ExecutionRecovered:1",
            at,
        )?;
        let event =
            ExecutionRecovered::decode(r.payload.as_slice()).map_err(|_| Error::InvalidInput)?;
        if event.state.as_ref() != Some(&st) || message_field(&r.payload, 2)? != batch.state {
            return Err(Error::InvalidInput);
        }
        let fill = FillWire::decode(&message_field(&r.payload, 1)?)?;
        let f = fill.value();
        let p = f.recovery.as_ref().ok_or(Error::InvalidInput)?;
        if st.portfolio_id.is_empty()
            || f.order_id != st.order_id
            || f.venue != st.venue
            || f.venue_account_id != st.venue_account_id
            || f.instrument_id != st.instrument_id
            || f.side != st.side
            || f.fill_id.is_empty()
            || f.fill_id.len() > 1024
            || f.venue.is_empty()
            || !identity::domain(f)
            || !is_positive(f.quantity.as_ref())
            || p.case_id != status.case_id
            || p.mapping_version != status.mapping_version
            || p.source_cursor != status.source_cursor
            || p.payload_digest != batch.history_digest
            || p.execution_digest
                != format!(
                    "{:x}",
                    Sha256::digest(fill.without_recovery()?.journal_bytes()?)
                )
        {
            return Err(Error::InvalidInput);
        }
        if p.fee_approval.is_some() {
            fee_revision::terms(&fill)?;
        }
        let key = identity::execution_key(f);
        if key == f.fill_id || fills.insert(key, fill).is_some() {
            return Err(Error::IdentityConflict);
        }
    }
    let missing: BTreeSet<_> = batch.missing.iter().cloned().collect();
    let mut posted = BTreeMap::new();
    for f in &batch.posted {
        if f.value().order_id != st.order_id
            || posted
                .insert(identity::execution_key(f.value()), f)
                .is_some()
        {
            return Err(Error::IdentityConflict);
        }
    }
    if missing.len() != batch.missing.len()
        || missing.len() + posted.len() != fills.len()
        || missing
            .iter()
            .any(|k| !fills.contains_key(k) || posted.contains_key(k))
        || posted.keys().any(|k| !fills.contains_key(k))
    {
        return Err(Error::InvalidInput);
    }
    for (key, previous) in &posted {
        let next = &fills[key];
        if !identity::same_execution(previous.value(), next.value())
            && (next
                .value()
                .recovery
                .as_ref()
                .and_then(|p| p.fee_approval.as_ref())
                .is_none()
                || !identity::same_except_fee(previous.value(), next.value()))
        {
            return Err(Error::IdentityConflict);
        }
    }
    let mut tx = scoped(client, tenant).await?;
    let tables=tx.query_one("SELECT count(*) FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relname IN ('execution_recovery_cases','execution_recovery_targets','execution_recovery_history') AND relrowsecurity AND relforcerowsecurity",&[]).await?;
    if tables.get::<_, i64>(0) != 3 {
        return Err(Error::UnsafeRole);
    }
    // Immutable history was validated by the trusted preparer; bind its stored
    // digest and mapping here without re-querying a venue or accepting a new history.
    let history=tx.query_one("SELECT EXISTS(SELECT 1 FROM execution_recovery_history WHERE case_id=$1 AND mapping_version=$2 AND payload_digest=$3)",&[&status.case_id,&status.mapping_version,&batch.history_digest]).await?;
    if !history.get::<_, bool>(0) {
        return Err(Error::InvalidInput);
    }
    let mut claims: Vec<_> = missing.iter().map(|k| &fills[k]).collect();
    claims.sort_by(|a, b| a.value().fill_id.cmp(&b.value().fill_id));
    for fill in claims {
        let f = fill.value();
        if claim(&tx, &st.order_id, &identity::execution_key(f), &f.fill_id).await? == 0 {
            duplicate(&tx, Some(f)).await?;
            return Err(Error::Conflict);
        }
        journal(&tx, &st.order_id, &f.fill_id, f, &fill.journal_bytes()?).await?;
    }
    {
        let mut locked =
            fee_journal::cas_order(&mut tx, tenant, &batch.state, batch.order_version).await?;
        for (key, fill) in &fills {
            if fill
                .value()
                .recovery
                .as_ref()
                .and_then(|p| p.fee_approval.as_ref())
                .is_some()
            {
                locked
                    .record_fee_revision(posted.get(key).ok_or(Error::FeeRevision)?, fill)
                    .await?;
            }
        }
    }
    let checkpoint = count as i32;
    let acknowledgements = checkpoint * 2;
    let changed=tx.execute("UPDATE execution_recovery_cases SET checkpoint=$1,expected_ack_count=$2,status=$3,reason=$4,version=version+1,updated_at=$5 WHERE case_id=$6 AND version=$7 AND status='investigating' AND checkpoint=0 AND order_id=$8 AND mapping_version=$9 AND source_cursor=$10 AND payload_digest=$11",&[&checkpoint,&acknowledgements,&status.status,&status.reason,&at,&status.case_id,&batch.case_version,&st.order_id,&status.mapping_version,&status.source_cursor,&status.payload_digest]).await?;
    if changed != 1 {
        return Err(Error::Conflict);
    }
    for (key, fill) in &fills {
        let p = fill.value().recovery.as_ref().ok_or(Error::InvalidInput)?;
        tx.execute("INSERT INTO execution_recovery_targets (case_id,execution_key,payload_digest,execution_digest) VALUES ($1,$2,$3,$4)",&[&status.case_id,key,&batch.history_digest,&p.execution_digest]).await?;
    }
    enqueue(&tx, &batch.records).await?;
    commit(tx).await
}

#[cfg(test)]
mod tests {
    use super::*;
    use kanz_order_core::proto::order::v1::Fill;
    #[test]
    fn metadata_and_posted_share_one_byte_budget() {
        let raw = Fill {
            fill_id: "x".repeat(750_000),
            ..Default::default()
        }
        .encode_to_vec();
        let fill = FillWire::decode(&raw).expect("bounded canonical fill");
        let state = OrderState {
            order_id: "order".into(),
            ..Default::default()
        }
        .encode_to_vec();
        let record = Record {
            tenant: String::new(),
            partition_key: String::new(),
            subject: String::new(),
            event_type: String::new(),
            event_class: 1,
            schema_version: 1,
            domain: String::new(),
            payload_schema_ref: String::new(),
            event_time: UNIX_EPOCH,
            correlation_id: "m".repeat(2 * MAX_BYTES),
            causation_id: String::new(),
            trace_context: String::new(),
            payload: vec![0; MAX_BYTES / 2],
        };
        let batch = PreparedRecovery {
            state,
            order_version: 0,
            case_version: 0,
            history_digest: "a".repeat(64),
            missing: vec![],
            posted: vec![fill.clone(), fill.clone(), fill],
            records: vec![record],
        };
        // Each formerly separate budget passes; their true union must refuse.
        assert!(validate_batch(&batch.state, &batch.records, MAX_FILLS + 1, MAX_BATCH).is_ok());
        assert!(
            batch.state.len()
                + batch.records[0].payload.len()
                + batch
                    .posted
                    .iter()
                    .map(|f| f.original().len())
                    .sum::<usize>()
                < MAX_BATCH
        );
        assert!(matches!(
            projection(&batch),
            Err(Error::InvalidInput) | Err(Error::Capacity)
        ));
    }
}

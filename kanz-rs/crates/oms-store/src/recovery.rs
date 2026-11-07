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
    commit_batch(client, tenant, Batch::Prepared(batch), st).await
}

/// Concrete immutable recipes retain the bounded input history once and expand
/// one FACT at a time. This is not a callback, permit, or externally prepared
/// financial projection. The coordinator owns evidence validation.
pub struct StreamedRecovery {
    pub state: Vec<u8>,
    pub order_version: i64,
    pub case_version: i64,
    pub history_digest: String,
    pub missing: Vec<String>,
    pub executions: Vec<super::recovery_wire::Execution>,
    pub status: Record,
}
enum Batch<'a> {
    Prepared(&'a PreparedRecovery),
    Streamed(&'a StreamedRecovery),
}
impl Batch<'_> {
    fn state(&self) -> &[u8] {
        match self {
            Self::Prepared(b) => &b.state,
            Self::Streamed(b) => &b.state,
        }
    }
    fn status(&self) -> Result<&Record, Error> {
        match self {
            Self::Prepared(b) => b.records.last().ok_or(Error::InvalidInput),
            Self::Streamed(b) => Ok(&b.status),
        }
    }
    fn count(&self) -> usize {
        match self {
            Self::Prepared(b) => b.records.len() - 1,
            Self::Streamed(b) => b.executions.len(),
        }
    }
    fn missing(&self) -> &[String] {
        match self {
            Self::Prepared(b) => &b.missing,
            Self::Streamed(b) => &b.missing,
        }
    }
    fn digest(&self) -> &str {
        match self {
            Self::Prepared(b) => &b.history_digest,
            Self::Streamed(b) => &b.history_digest,
        }
    }
    fn versions(&self) -> (i64, i64) {
        match self {
            Self::Prepared(b) => (b.order_version, b.case_version),
            Self::Streamed(b) => (b.order_version, b.case_version),
        }
    }
    fn record(&self, index: usize) -> Result<std::borrow::Cow<'_, Record>, Error> {
        match self {
            Self::Prepared(b) => Ok(std::borrow::Cow::Borrowed(&b.records[index])),
            Self::Streamed(b) => Ok(std::borrow::Cow::Owned(
                b.executions[index].record(&b.state, &b.status)?,
            )),
        }
    }
    fn fill(&self, index: usize) -> Result<FillWire, Error> {
        match self {
            Self::Prepared(b) => FillWire::decode(&message_field(&b.records[index].payload, 1)?),
            Self::Streamed(b) => b.executions[index].materialize(),
        }
    }
}
pub async fn commit_streamed_recovery(
    client: &mut Client,
    tenant: &str,
    batch: &StreamedRecovery,
) -> Result<(), Error> {
    validate_missing_keys(&batch.missing)?;
    if batch.executions.len() > MAX_FILLS
        || batch.missing.len() > MAX_FILLS
        || batch.history_digest.len() != 64
        || !batch.history_digest.bytes().all(|b| b.is_ascii_hexdigit())
    {
        return Err(Error::InvalidInput);
    }
    super::recovery_wire::validate_resident(&batch.executions)?;
    let st = validate_batch(
        &batch.state,
        std::slice::from_ref(&batch.status),
        1,
        2 * MAX_BYTES,
    )?;
    commit_batch(client, tenant, Batch::Streamed(batch), st).await
}
fn validate_missing_keys(missing: &[String]) -> Result<(), Error> {
    // Preflight before commit_batch clones keys: at most 8000 canonical
    // execution identities, each at most 256 bytes (2,048,000 bytes total).
    if missing.len() > MAX_FILLS || missing.iter().any(|key| key.len() > 256) {
        return Err(Error::Capacity);
    }
    Ok(())
}
async fn commit_batch(
    client: &mut Client,
    tenant: &str,
    batch: Batch<'_>,
    st: OrderState,
) -> Result<(), Error> {
    let status_record = batch.status()?;
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
    let count = batch.count();
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
    for index in 0..count {
        let r = batch.record(index)?;
        let r = r.as_ref();
        if matches!(&batch, Batch::Streamed(_)) {
            validate_batch(batch.state(), std::slice::from_ref(r), 1, 2 * MAX_BYTES)?;
        }
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
        if event.state.as_ref() != Some(&st) || message_field(&r.payload, 2)? != batch.state() {
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
            || p.payload_digest != batch.digest()
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
        if key == f.fill_id || fills.insert(key, index).is_some() {
            return Err(Error::IdentityConflict);
        }
    }
    let missing: BTreeSet<_> = batch.missing().iter().cloned().collect();
    let mut posted = BTreeMap::new();
    for f in match &batch {
        Batch::Prepared(b) => b.posted.as_slice(),
        Batch::Streamed(_) => &[],
    } {
        if f.value().order_id != st.order_id
            || posted
                .insert(identity::execution_key(f.value()), f)
                .is_some()
        {
            return Err(Error::IdentityConflict);
        }
    }
    if missing.len() != batch.missing().len()
        || (matches!(&batch, Batch::Prepared(_)) && missing.len() + posted.len() != fills.len())
        || missing
            .iter()
            .any(|k| !fills.contains_key(k) || posted.contains_key(k))
        || posted.keys().any(|k| !fills.contains_key(k))
    {
        return Err(Error::InvalidInput);
    }
    for (key, previous) in &posted {
        let next = batch.fill(fills[key])?;
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
    let history=tx.query_one("SELECT EXISTS(SELECT 1 FROM execution_recovery_history WHERE case_id=$1 AND mapping_version=$2 AND payload_digest=$3)",&[&status.case_id,&status.mapping_version,&batch.digest()]).await?;
    if !history.get::<_, bool>(0) {
        return Err(Error::InvalidInput);
    }
    let mut claims = Vec::with_capacity(missing.len());
    for key in &missing {
        let index = fills[key];
        let fill = batch.fill(index)?;
        claims.push((fill.value().fill_id.clone(), index));
    }
    claims.sort_by(|a, b| a.0.cmp(&b.0));
    for (_, index) in claims {
        let fill = batch.fill(index)?;
        let f = fill.value();
        if claim(&tx, &st.order_id, &identity::execution_key(f), &f.fill_id).await? == 0 {
            duplicate(&tx, Some(f)).await?;
            return Err(Error::Conflict);
        }
        journal(&tx, &st.order_id, &f.fill_id, f, &fill.journal_bytes()?).await?;
    }
    {
        let mut locked =
            fee_journal::cas_order(&mut tx, tenant, batch.state(), batch.versions().0).await?;
        for (key, index) in &fills {
            let fill = batch.fill(*index)?;
            if missing.contains(key) {
                if fill
                    .value()
                    .recovery
                    .as_ref()
                    .and_then(|p| p.fee_approval.as_ref())
                    .is_some()
                {
                    return Err(Error::FeeRevision);
                }
                continue;
            }
            // Re-read the durable fee head after acquiring the order CAS lock.
            // The readonly preparation snapshot is not a substitute for this fence.
            let previous = match &batch {
                Batch::Prepared(_) => (**posted.get(key).ok_or(Error::FeeRevision)?).clone(),
                Batch::Streamed(_) => locked.execution_head(&fill).await?,
            };
            if fill
                .value()
                .recovery
                .as_ref()
                .and_then(|p| p.fee_approval.as_ref())
                .is_some()
            {
                locked.record_fee_revision(&previous, &fill).await?;
            } else if !identity::same_execution(previous.value(), fill.value()) {
                return Err(Error::IdentityConflict);
            }
        }
    }
    let checkpoint = count as i32;
    let acknowledgements = checkpoint * 2;
    let changed=tx.execute("UPDATE execution_recovery_cases SET checkpoint=$1,expected_ack_count=$2,status=$3,reason=$4,version=version+1,updated_at=$5 WHERE case_id=$6 AND version=$7 AND status='investigating' AND checkpoint=0 AND order_id=$8 AND mapping_version=$9 AND source_cursor=$10 AND payload_digest=$11",&[&checkpoint,&acknowledgements,&status.status,&status.reason,&at,&status.case_id,&batch.versions().1,&st.order_id,&status.mapping_version,&status.source_cursor,&status.payload_digest]).await?;
    if changed != 1 {
        return Err(Error::Conflict);
    }
    for (key, index) in &fills {
        let fill = batch.fill(*index)?;
        let p = fill.value().recovery.as_ref().ok_or(Error::InvalidInput)?;
        tx.execute("INSERT INTO execution_recovery_targets (case_id,execution_key,payload_digest,execution_digest) VALUES ($1,$2,$3,$4)",&[&status.case_id,key,&batch.digest(),&p.execution_digest]).await?;
    }
    for index in 0..count {
        let record = batch.record(index)?;
        enqueue(&tx, std::slice::from_ref(record.as_ref())).await?;
    }
    enqueue(&tx, std::slice::from_ref(status_record)).await?;
    commit(tx).await
}

#[cfg(test)]
mod tests {
    use super::*;
    use kanz_order_core::proto::order::v1::Fill;
    #[test]
    fn streamed_missing_keys_are_bounded_before_copy() {
        assert!(validate_missing_keys(&vec!["k".repeat(256); MAX_FILLS]).is_ok());
        assert!(matches!(
            validate_missing_keys(&["k".repeat(257)]),
            Err(Error::Capacity)
        ));
        assert!(matches!(
            validate_missing_keys(&vec![String::new(); MAX_FILLS + 1]),
            Err(Error::Capacity)
        ));
    }
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

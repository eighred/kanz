//! Durable acknowledgement only. Go owns authenticated FACT/subject-to-book admission.
use super::{commit, enqueue, recovery, scoped, validate_batch, Error, Record, MAX_BYTES};
use crate::diagnostics::{Stage, Timer};
use kanz_order_core::proto::order::v1::{
    ExecutionRecoveryApplied, ExecutionRecoveryRecorded, OrderState,
};
use prost::Message;
use tokio_postgres::{types::Type, Client};

/// Construct only after the existing Go handler validates the publisher's subject.
#[derive(Clone, Copy)]
pub enum Book {
    Ledger,
    Position,
}
impl Book {
    fn as_str(self) -> &'static str {
        match self {
            Self::Ledger => "ledger",
            Self::Position => "position",
        }
    }
}

/// The prepared corrected FACT is mandatory, even for an acknowledgement that
/// turns out not to be final. It is never enqueued unless every target is booked.
/// Envelope and payload metadata are bound before any write, including portfolio
/// and venue fields from the order loaded inside the case lock. No approval is granted here.
pub async fn acknowledge_recovery(
    client: &mut Client,
    tenant: &str,
    ack: &ExecutionRecoveryApplied,
    book: Book,
    corrected: &Record,
) -> Result<(), Error> {
    for (s, bound) in [
        (&ack.case_id, 256),
        (&ack.order_id, 256),
        (&ack.execution_key, 256),
        (&ack.payload_digest, 64),
        (&ack.execution_digest, 64),
    ] {
        if s.is_empty() || s.len() > bound {
            return Err(Error::InvalidInput);
        }
    }
    // Reuse the common complete record budget before beginning a transaction.
    let dummy = OrderState {
        order_id: ack.order_id.clone(),
        ..Default::default()
    }
    .encode_to_vec();
    validate_batch(&dummy, std::slice::from_ref(corrected), 1, MAX_BYTES)?;
    let status = ExecutionRecoveryRecorded::decode(corrected.payload.as_slice())
        .map_err(|_| Error::InvalidInput)?;
    let at = recovery::time(status.recorded_at.as_ref().ok_or(Error::InvalidInput)?)?;
    recovery::fact(
        corrected,
        tenant,
        &ack.order_id,
        "order.order.recovery_recorded",
        "order.v1.ExecutionRecoveryRecorded:1",
        at,
    )?;
    if status.case_id != ack.case_id
        || status.order_id != ack.order_id
        || status.status != "corrected"
        || !status.reason.is_empty()
        || status.checkpoint > 8000
    {
        return Err(Error::InvalidInput);
    }
    let tx = scoped(client, tenant).await?;
    let tables=tx.query_typed_one("SELECT count(*) FROM pg_class WHERE oid IN ('execution_recovery_cases'::regclass,'execution_recovery_targets'::regclass,'execution_recovery_acks'::regclass) AND relrowsecurity AND relforcerowsecurity",&[]).await?;
    if tables.get::<_, i64>(0) != 3 {
        return Err(Error::UnsafeRole);
    }
    let limit = MAX_BYTES as i64;
    // Do not transfer unbounded stored TEXT, even from a corrupt row.
    let row=tx.query_typed_opt("SELECT CASE WHEN octet_length(order_id)::bigint+octet_length(COALESCE(mapping_version,''))+octet_length(source_cursor)+octet_length(payload_digest)+octet_length(status)<=$2 THEN ARRAY[order_id,COALESCE(mapping_version,''),source_cursor,payload_digest,status] END,checkpoint,expected_ack_count FROM execution_recovery_cases WHERE case_id=$1 FOR UPDATE",&[(&ack.case_id, Type::TEXT), (&limit, Type::INT8)]).await?.ok_or(Error::NotFound)?;
    let fields: Vec<String> = row
        .get::<_, Option<Vec<String>>>(0)
        .ok_or(Error::Capacity)?;
    let checkpoint: i32 = row.get(1);
    let expected: i32 = row.get(2);
    if fields[0] != ack.order_id
        || fields[1] != status.mapping_version
        || fields[2] != status.source_cursor
        || fields[3] != status.payload_digest
        || checkpoint < 0
        || checkpoint as u32 != status.checkpoint
    {
        return Err(Error::IdentityConflict);
    }
    // Canonical preparation writes one target per checkpoint and two books per
    // target. Corrupt counts must not become an indefinitely pending success.
    let target_timer = Timer::new(Stage::AckTargets);
    let targets: i64 = tx
        .query_typed_one(
            "SELECT count(*) FROM execution_recovery_targets WHERE case_id=$1",
            &[(&ack.case_id, Type::TEXT)],
        )
        .await?
        .get(0);
    drop(target_timer);
    if checkpoint == 0 || expected != checkpoint * 2 || targets != i64::from(checkpoint) {
        return Err(Error::IdentityConflict);
    }
    let matches=tx.query_typed_one("SELECT EXISTS(SELECT 1 FROM execution_recovery_targets WHERE case_id=$1 AND execution_key=$2 AND payload_digest=$3 AND execution_digest=$4)",&[(&ack.case_id, Type::TEXT), (&ack.execution_key, Type::TEXT), (&ack.payload_digest, Type::TEXT), (&ack.execution_digest, Type::TEXT)]).await?;
    if !matches.get::<_, bool>(0) {
        return Err(Error::IdentityConflict);
    }
    let row = tx
        .query_typed_opt(
            "SELECT CASE WHEN octet_length(state)::bigint<=$2 THEN state END FROM orders WHERE order_id=$1",
            &[(&ack.order_id, Type::TEXT), (&limit, Type::INT8)],
        )
        .await?
        .ok_or(Error::NotFound)?;
    let raw: Vec<u8> = row.get::<_, Option<Vec<u8>>>(0).ok_or(Error::Capacity)?;
    let state = validate_batch(&raw, std::slice::from_ref(corrected), 1, MAX_BYTES)?;
    if state.order_id != ack.order_id
        || status.portfolio_id != state.portfolio_id
        || status.venue != state.venue
        || status.venue_account_id != state.venue_account_id
    {
        return Err(Error::IdentityConflict);
    }
    if fields[4] == "corrected" {
        return commit(tx).await;
    }
    tx.execute("INSERT INTO execution_recovery_acks (case_id,execution_key,book) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING",&[&ack.case_id,&ack.execution_key,&book.as_str()]).await?;
    let received_timer = Timer::new(Stage::AckReceived);
    let received: i64 = tx
        .query_typed_one(
            "SELECT count(*) FROM execution_recovery_acks WHERE case_id=$1",
            &[(&ack.case_id, Type::TEXT)],
        )
        .await?
        .get(0);
    drop(received_timer);
    if expected == 0 || received != i64::from(expected) {
        return commit(tx).await;
    }
    tx.execute("UPDATE execution_recovery_cases SET status='corrected',reason='',version=version+1,updated_at=$2 WHERE case_id=$1",&[&ack.case_id,&at]).await?;
    enqueue(&tx, std::slice::from_ref(corrected)).await?;
    commit(tx).await
}

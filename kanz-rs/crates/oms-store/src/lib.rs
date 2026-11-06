#![forbid(unsafe_code)]
//! Existing OMS Save transaction semantics; no network listener, runtime configuration,
//! admission, recovery authorization or production caller.
pub mod fee_journal;
pub mod fee_revision;
pub mod fee_subject;
pub mod fee_wire;
mod go_unicode;
pub mod identity;
pub mod queries;
pub mod recovery;
use identity::{execution_key, same_execution};
use kanz_oms_fold_shadow::proto::order::v1::{Fill, OrderFilled, OrderPartiallyFilled, OrderState};
use prost::Message;
use std::time::SystemTime;
use tokio_postgres::{Client, Transaction};

pub const MAX_BYTES: usize = 1024 * 1024;
#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    InvalidInput,
    Exists,
    NotFound,
    Capacity,
    UnsafeRole,
    Conflict,
    FillApplied,
    IdentityConflict,
    FeeRevision,
    UnownedOutbox,
    Database,
    CommitUnconfirmed,
}
impl From<tokio_postgres::Error> for Error {
    fn from(_: tokio_postgres::Error) -> Self {
        Self::Database
    }
}
impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}
impl std::error::Error for Error {}

/// Stored FACT metadata, not a new public command or canonical financial model.
pub struct Record {
    pub tenant: String,
    pub partition_key: String,
    pub subject: String,
    pub event_type: String,
    pub event_class: i32,
    pub schema_version: i32,
    pub domain: String,
    pub payload_schema_ref: String,
    pub event_time: SystemTime,
    pub correlation_id: String,
    pub causation_id: String,
    pub trace_context: String,
    pub payload: Vec<u8>,
}
// Preserve the canonical nested fill bytes as well as the entire FACT. In
// particular prost decoding must not erase forward-compatible unknown fields.
fn fill_wire(wire: &[u8]) -> Result<Vec<u8>, Error> {
    message_field(wire, 2)
}
fn message_field(mut wire: &[u8], field: u64) -> Result<Vec<u8>, Error> {
    fn varint(wire: &mut &[u8]) -> Result<u64, Error> {
        let mut out = 0u64;
        for i in 0..10 {
            let (&b, tail) = wire.split_first().ok_or(Error::InvalidInput)?;
            *wire = tail;
            if i == 9 && b > 1 {
                return Err(Error::InvalidInput);
            }
            out |= u64::from(b & 127) << (7 * i);
            if b < 128 {
                return Ok(out);
            }
        }
        Err(Error::InvalidInput)
    }
    let mut found = None;
    while !wire.is_empty() {
        let tag = varint(&mut wire)?;
        if tag >> 3 == 0 {
            return Err(Error::InvalidInput);
        }
        let n = match tag & 7 {
            0 => {
                varint(&mut wire)?;
                continue;
            }
            1 => 8,
            5 => 4,
            2 => usize::try_from(varint(&mut wire)?).map_err(|_| Error::InvalidInput)?,
            _ => return Err(Error::InvalidInput),
        };
        if wire.len() < n {
            return Err(Error::InvalidInput);
        }
        if tag == (field << 3 | 2) {
            if found.is_some() {
                return Err(Error::InvalidInput);
            }
            found = Some(wire[..n].to_vec());
        }
        wire = &wire[n..];
    }
    found.ok_or(Error::InvalidInput)
}
fn announced(
    order_id: &str,
    fill_id: &str,
    records: &[Record],
) -> Result<Option<(Fill, Vec<u8>)>, Error> {
    let mut found = None;
    for r in records {
        let fill = match r.event_type.as_str() {
            "order.order.filled" => {
                OrderFilled::decode(r.payload.as_slice())
                    .map_err(|_| Error::InvalidInput)?
                    .fill
            }
            "order.order.partially_filled" => {
                OrderPartiallyFilled::decode(r.payload.as_slice())
                    .map_err(|_| Error::InvalidInput)?
                    .fill
            }
            // Recovery is a different transaction owner, deliberately not exposed.
            "order.order.execution_recovered" => return Err(Error::InvalidInput),
            _ => continue,
        };
        let f = fill.ok_or(Error::InvalidInput)?;
        if f.order_id != order_id || f.fill_id != fill_id || found.is_some() {
            return Err(Error::InvalidInput);
        }
        found = Some((f, fill_wire(&r.payload)?));
    }
    Ok(found)
}
async fn duplicate(tx: &Transaction<'_>, fill: Option<&Fill>) -> Result<(), Error> {
    let Some(fill) = fill else { return Ok(()) };
    let row = if execution_key(fill) != fill.fill_id {
        tx.query_opt("SELECT fill FROM order_executions WHERE venue=$1 AND venue_account_id=$2 AND instrument_id=$3 AND venue_execution_id=$4",&[&fill.venue,&fill.venue_account_id,&fill.instrument_id,&fill.venue_execution_id]).await?
    } else {
        tx.query_opt(
            "SELECT fill FROM order_executions WHERE order_id=$1 AND fill_id=$2",
            &[&fill.order_id, &fill.fill_id],
        )
        .await?
    };
    let Some(row) = row else { return Ok(()) }; // Existing legacy claim without execution proof.
    let raw: Vec<u8> = row.get(0);
    if raw.len() > MAX_BYTES {
        return Err(Error::InvalidInput);
    }
    let original = fee_wire::FillWire::decode(&raw)?;
    if same_execution(original.value(), fill) {
        return Ok(());
    }
    let revised = fee_journal::head(tx, &original).await?;
    if !same_execution(revised.value(), fill) {
        return Err(Error::IdentityConflict);
    }
    Ok(())
}

fn validate_write(state: &[u8], records: &[Record]) -> Result<OrderState, Error> {
    validate_batch(state, records, 64, MAX_BYTES)
}
fn validate_batch(
    state: &[u8],
    records: &[Record],
    max_records: usize,
    max_bytes: usize,
) -> Result<OrderState, Error> {
    let bytes = records.iter().try_fold(state.len(), |sum, r| {
        [
            &r.tenant,
            &r.partition_key,
            &r.subject,
            &r.event_type,
            &r.domain,
            &r.payload_schema_ref,
            &r.correlation_id,
            &r.causation_id,
            &r.trace_context,
        ]
        .into_iter()
        .try_fold(sum.checked_add(r.payload.len())?, |n, s| {
            n.checked_add(s.len())
        })
    });
    if state.len() > MAX_BYTES || records.len() > max_records || bytes.is_none_or(|n| n > max_bytes)
    {
        return Err(Error::InvalidInput);
    }
    let st = OrderState::decode(state).map_err(|_| Error::InvalidInput)?;
    if st.order_id.is_empty() {
        return Err(Error::InvalidInput);
    }
    Ok(st)
}
async fn scoped<'a>(client: &'a mut Client, tenant: &str) -> Result<Transaction<'a>, Error> {
    let tx = client.transaction().await?;
    scope_existing(&tx, tenant).await?;
    Ok(tx)
}
async fn scope_existing(tx: &Transaction<'_>, tenant: &str) -> Result<(), Error> {
    if tenant.is_empty() || tenant.len() > 256 {
        return Err(Error::InvalidInput);
    }
    let role = tx
        .query_one(
            "SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user",
            &[],
        )
        .await?;
    if role.get::<_, bool>(0) || role.get::<_, bool>(1) {
        return Err(Error::UnsafeRole);
    }
    tx.query_one("SELECT set_config('app.tenant_id',$1,true)", &[&tenant])
        .await?;
    // Every write uses the canonical tables, which must retain forced tenant RLS.
    let tables=tx.query_one("SELECT count(*) FROM pg_class WHERE oid IN ('orders'::regclass,'order_fills'::regclass,'order_executions'::regclass,'outbox'::regclass,'execution_fee_revisions'::regclass) AND relrowsecurity AND relforcerowsecurity",&[]).await?;
    if tables.get::<_, i64>(0) != 5 {
        return Err(Error::UnsafeRole);
    }
    Ok(())
}
async fn enqueue(tx: &Transaction<'_>, records: &[Record]) -> Result<(), Error> {
    for r in records {
        if r.tenant.is_empty() {
            return Err(Error::InvalidInput);
        }
        let count=tx.execute("INSERT INTO outbox (producer_service,envelope_tenant_id,partition_key,subject,event_type,event_class,schema_version,domain,payload_schema_ref,event_time,correlation_id,causation_id,trace_context,payload) SELECT 'oms',$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13 WHERE NOT EXISTS (SELECT 1 FROM outbox WHERE published_at IS NULL AND producer_service IS NULL)",&[&r.tenant,&r.partition_key,&r.subject,&r.event_type,&r.event_class,&r.schema_version,&r.domain,&r.payload_schema_ref,&r.event_time,&r.correlation_id,&r.causation_id,&r.trace_context,&r.payload]).await?;
        if count != 1 {
            return Err(Error::UnownedOutbox);
        }
    }
    Ok(())
}

/// Atomic storage admission only. The caller still owns institutional permission.
pub async fn create(
    client: &mut Client,
    tenant: &str,
    state: &[u8],
    records: &[Record],
) -> Result<(), Error> {
    let st = validate_write(state, records)?;
    let tx = scoped(client, tenant).await?;
    let count=tx.execute("INSERT INTO orders (tenant_id,order_id,status,state,portfolio_id,parent_order_id) VALUES (current_setting('app.tenant_id'),$1,$2,$3,$4,$5) ON CONFLICT (tenant_id,order_id) DO NOTHING",&[&st.order_id,&st.status,&state,&st.portfolio_id,&st.parent_order_id]).await?;
    if count == 0 {
        return Err(Error::Exists);
    }
    enqueue(&tx, records).await?;
    commit(tx).await
}
/// Raw canonical state and its version from one row/snapshot; no normalization.
pub struct Loaded {
    pub state: Vec<u8>,
    pub version: i64,
}
pub async fn load(client: &mut Client, tenant: &str, order_id: &str) -> Result<Loaded, Error> {
    let tx = scoped(client, tenant).await?;
    let row = tx
        .query_opt(
            "SELECT CASE WHEN octet_length(state)::bigint<=$2 THEN state END,version FROM orders WHERE order_id=$1",
            &[&order_id,&(MAX_BYTES as i64)],
        )
        .await?
        .ok_or(Error::NotFound)?;
    let state: Vec<u8> = row
        .try_get::<_, Option<Vec<u8>>>(0)?
        .ok_or(Error::Capacity)?;
    let version: i64 = row.try_get(1)?;
    if state.len() > MAX_BYTES {
        return Err(Error::Capacity);
    }
    OrderState::decode(state.as_slice()).map_err(|_| Error::InvalidInput)?;
    tx.rollback().await?;
    Ok(Loaded { state, version })
}
/// Claims are only permission to skip a fold. Any read failure/capacity error
/// discards the whole result; absence must never be inferred from a prefix.
pub async fn applied_fills(
    client: &mut Client,
    tenant: &str,
    order_id: &str,
) -> Result<std::collections::BTreeSet<String>, Error> {
    let tx = scoped(client, tenant).await?;
    if order_id.is_empty() {
        tx.rollback().await?;
        return Ok(Default::default());
    }
    // The SQL projection checks the whole bounded selection before transferring
    // TEXT. Checking length only after query() would already allocate oversized
    // rows in the client. A byte-excess selection returns NULLs, never a prefix.
    let rows=tx.query("SELECT CASE WHEN sum(octet_length(id)) OVER ()<=$2 THEN id END FROM (SELECT COALESCE(NULLIF(raw_fill_id,''),fill_id) AS id FROM order_fills WHERE order_id=$1 LIMIT 100001) selected",&[&order_id,&(MAX_BYTES as i64)]).await?;
    if rows.len() > 100000 {
        return Err(Error::Capacity);
    }
    let mut total = 0usize;
    let mut claims = std::collections::BTreeSet::new();
    for row in rows {
        let id: String = row
            .try_get::<_, Option<String>>(0)?
            .ok_or(Error::Capacity)?;
        total = total.checked_add(id.len()).ok_or(Error::Capacity)?;
        if total > MAX_BYTES {
            return Err(Error::Capacity);
        }
        claims.insert(id);
    }
    tx.rollback().await?;
    Ok(claims)
}

/// Trusted repository boundary: caller owns admission and FACT construction.
/// Raw protobuf bytes are stored verbatim, including optional/unknown fields.
/// Tenant is transaction-local, never inferred from a payload or retained on a pool.
pub async fn save(
    client: &mut Client,
    tenant: &str,
    state: &[u8],
    expected_version: i64,
    records: &[Record],
    fill_id: &str,
) -> Result<(), Error> {
    if fill_id.len() > 1024 {
        return Err(Error::InvalidInput);
    }
    let st = validate_write(state, records)?;
    let tx = scoped(client, tenant).await?;
    if !fill_id.is_empty() {
        let fill = announced(&st.order_id, fill_id, records)?;
        let key = fill
            .as_ref()
            .map_or_else(|| fill_id.to_owned(), |f| execution_key(&f.0));
        if key != fill_id {
            if let Some(row) = tx
                .query_opt(
                    "SELECT order_id FROM order_fills WHERE fill_id=$1",
                    &[&fill_id],
                )
                .await?
            {
                return Err(if row.get::<_, String>(0) == st.order_id {
                    Error::FillApplied
                } else {
                    Error::IdentityConflict
                });
            }
        }
        let count = claim(&tx, &st.order_id, &key, fill_id).await?;
        if count == 0 {
            duplicate(&tx, fill.as_ref().map(|f| &f.0)).await?;
            return Err(Error::FillApplied);
        }
        if let Some((fill, raw)) = fill {
            journal(&tx, &st.order_id, fill_id, &fill, &raw).await?;
        }
    }
    cas(&tx, &st, state, expected_version).await?;
    enqueue(&tx, records).await?;
    commit(tx).await
}
async fn cas(
    tx: &Transaction<'_>,
    st: &OrderState,
    state: &[u8],
    expected_version: i64,
) -> Result<(), Error> {
    let count=tx.execute("INSERT INTO orders (tenant_id,order_id,status,state,portfolio_id,parent_order_id) VALUES (current_setting('app.tenant_id'),$1,$2,$3,$5,$6) ON CONFLICT (tenant_id,order_id) DO UPDATE SET status=EXCLUDED.status,state=EXCLUDED.state,portfolio_id=EXCLUDED.portfolio_id,parent_order_id=EXCLUDED.parent_order_id,version=orders.version+1,updated_at=now() WHERE orders.version=$4",&[&st.order_id,&st.status,&state,&expected_version,&st.portfolio_id,&st.parent_order_id]).await?;
    if count == 0 {
        return Err(Error::Conflict);
    }
    Ok(())
}
async fn commit(tx: Transaction<'_>) -> Result<(), Error> {
    // Connection loss around COMMIT is not proof of rollback. Callers must
    // reconcile durable state before retrying; never report a fabricated success.
    tx.commit().await.map_err(|_| Error::CommitUnconfirmed)?;
    Ok(())
}

async fn claim(tx: &Transaction<'_>, order: &str, key: &str, alias: &str) -> Result<u64, Error> {
    Ok(tx.execute("INSERT INTO order_fills (tenant_id,order_id,fill_id,raw_fill_id) VALUES (current_setting('app.tenant_id'),$1,$2,$3) ON CONFLICT (tenant_id,fill_id) DO NOTHING",&[&order,&key,&alias]).await?)
}
async fn journal(
    tx: &Transaction<'_>,
    order: &str,
    alias: &str,
    fill: &Fill,
    raw: &[u8],
) -> Result<(), Error> {
    tx.execute("INSERT INTO order_executions (order_id,fill_id,venue,venue_account_id,instrument_id,venue_execution_id,fill) VALUES ($1,$2,$3,$4,$5,$6,$7)",&[&order,&alias,&fill.venue,&fill.venue_account_id,&fill.instrument_id,&fill.venue_execution_id,&raw]).await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn nested_wire_preservation_and_ambiguity() {
        let bytes = [18, 3, 0x78, 0x81, 1];
        assert_eq!(fill_wire(&bytes).unwrap(), vec![0x78, 0x81, 1]);
        for malformed in [
            &[18, 5, 1][..],
            &[18, 0, 18, 0][..],
            &[19, 20][..],
            &[0][..],
            &[255; 11][..],
        ] {
            assert_eq!(fill_wire(malformed), Err(Error::InvalidInput));
        }
    }
}

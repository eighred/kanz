//! Durable OMS position book qualification. No command admission or production wiring.
use crate::{
    commit, enqueue,
    fee_journal::{self, Book},
    fee_revision,
    fee_wire::FillWire,
    identity,
    position_math::{self as math, Lot},
    scoped, Error, Record, MAX_BYTES,
};
use kanz_oms_fold_shadow::{
    proto::{
        domain::v1::{PortfolioSnapshot, PortfolioState, PositionState},
        order::v1::Fill,
    },
    rational,
};
use num_traits::Zero;
use prost::Message;
use std::{
    collections::BTreeMap,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio_postgres::{Client, Transaction};
pub const MAX_ROWS: usize = 10000;
pub struct Applied {
    pub venue: PositionState,
    pub aggregate: PositionState,
}
fn timestamp(at: SystemTime) -> Result<prost_types::Timestamp, Error> {
    let t = prost_types::Timestamp::from(at);
    if !identity::timestamp(&t) {
        return Err(Error::InvalidInput);
    }
    Ok(t)
}
fn time(t: &prost_types::Timestamp) -> Result<SystemTime, Error> {
    if !identity::timestamp(t) {
        return Err(Error::InvalidInput);
    }
    let base = if t.seconds < 0 {
        UNIX_EPOCH.checked_sub(Duration::from_secs(t.seconds.unsigned_abs()))
    } else {
        UNIX_EPOCH.checked_add(Duration::from_secs(t.seconds as u64))
    };
    base.and_then(|b| b.checked_add(Duration::from_nanos(t.nanos as u64)))
        .ok_or(Error::InvalidInput)
}
fn currency(s: &str) -> Result<&str, Error> {
    if s.len() > 256 {
        return Err(Error::Capacity);
    }
    Ok(if s.is_empty() { "USD" } else { s })
}
async fn scope<'a>(client: &'a mut Client, tenant: &str) -> Result<Transaction<'a>, Error> {
    let tx = scoped(client, tenant).await?;
    let n:i64=tx.query_one("SELECT count(*) FROM pg_class WHERE oid IN ('positions'::regclass,'position_fills'::regclass,'position_execution_basis'::regclass,'position_execution_history'::regclass) AND relrowsecurity AND relforcerowsecurity",&[]).await?.try_get(0)?;
    if n != 4 {
        return Err(Error::UnsafeRole);
    }
    Ok(tx)
}
fn bounded_records(records: &[Record]) -> Result<(), Error> {
    let mut size = 0usize;
    if records.len() > 64 {
        return Err(Error::Capacity);
    }
    for r in records {
        size = size.checked_add(r.payload.len()).ok_or(Error::Capacity)?;
        for s in [
            &r.tenant,
            &r.subject,
            &r.partition_key,
            &r.event_type,
            &r.domain,
            &r.payload_schema_ref,
            &r.correlation_id,
            &r.causation_id,
            &r.trace_context,
        ] {
            size = size.checked_add(s.len()).ok_or(Error::Capacity)?;
        }
    }
    if size > MAX_BYTES {
        return Err(Error::Capacity);
    }
    Ok(())
}
async fn load(tx: &Transaction<'_>, portfolio: &str, fill: &Fill) -> Result<Lot, Error> {
    let row=tx.query_opt("SELECT CASE WHEN octet_length(quantity)<=4096 THEN quantity END,CASE WHEN octet_length(average_price)<=4096 THEN average_price END,CASE WHEN octet_length(realized_pnl)<=4096 THEN realized_pnl END FROM positions WHERE portfolio_id=$1 AND venue=$2 AND instrument_id=$3 FOR UPDATE",&[&portfolio,&fill.venue,&fill.instrument_id]).await?;
    let Some(row) = row else {
        return Ok(Lot::default());
    };
    let q: Option<String> = row.try_get(0)?;
    let a: Option<String> = row.try_get(1)?;
    let r: Option<String> = row.try_get(2)?;
    Lot::from_text(
        &q.ok_or(Error::Capacity)?,
        &a.ok_or(Error::Capacity)?,
        &r.ok_or(Error::Capacity)?,
    )
}
// Window bounds are evaluated in the same statement before any text transfers.
async fn lots(
    tx: &Transaction<'_>,
    portfolio: &str,
    instrument: Option<&str>,
) -> Result<Vec<(String, Vec<Lot>)>, Error> {
    let rows=tx.query("SELECT CASE WHEN n<=10000 AND bytes<=1048576 AND octet_length(instrument_id)<=4096 THEN instrument_id END,CASE WHEN n<=10000 AND bytes<=1048576 AND octet_length(quantity)<=4096 THEN quantity END,CASE WHEN n<=10000 AND bytes<=1048576 AND octet_length(average_price)<=4096 THEN average_price END,CASE WHEN n<=10000 AND bytes<=1048576 AND octet_length(realized_pnl)<=4096 THEN realized_pnl END FROM (SELECT instrument_id,quantity,average_price,realized_pnl,count(*) OVER() n,sum(octet_length(instrument_id)::bigint+octet_length(quantity)+octet_length(average_price)+octet_length(realized_pnl)) OVER() bytes FROM positions WHERE portfolio_id=$1 AND ($2::text IS NULL OR instrument_id=$2)) bounded ORDER BY instrument_id LIMIT 10001",&[&portfolio,&instrument]).await?;
    if rows.len() > MAX_ROWS {
        return Err(Error::Capacity);
    }
    let mut out: Vec<(String, Vec<Lot>)> = Vec::new();
    let mut indices: BTreeMap<String, usize> = BTreeMap::new();
    for row in rows {
        let get = |n| -> Result<String, Error> {
            row.try_get::<_, Option<String>>(n)?.ok_or(Error::Capacity)
        };
        // Keep the database collation's first-seen order, as Go Snapshot does.
        let key = get(0)?;
        let index = if let Some(index) = indices.get(&key) {
            *index
        } else {
            let index = out.len();
            indices.insert(key.clone(), index);
            out.push((key, Vec::new()));
            index
        };
        out[index]
            .1
            .push(Lot::from_text(&get(1)?, &get(2)?, &get(3)?)?);
    }
    Ok(out)
}
/// Announcer is synchronous and runs under the transaction/lock, just like Go.
/// It may only build Records; it must not publish, perform I/O, or acquire locks.
pub async fn apply<F>(
    client: &mut Client,
    tenant: &str,
    portfolio: &str,
    raw: &[u8],
    as_of: SystemTime,
    base_currency: &str,
    announce: F,
) -> Result<Applied, Error>
where
    F: FnOnce(&Applied) -> Result<Vec<Record>, Error>,
{
    if portfolio.len() > 4096 || raw.len() > MAX_BYTES {
        return Err(Error::Capacity);
    }
    let ccy = currency(base_currency)?;
    timestamp(as_of)?;
    let wire = FillWire::decode(raw)?;
    let f = wire.value();
    if [
        &f.fill_id,
        &f.order_id,
        &f.venue,
        &f.instrument_id,
        &f.venue_account_id,
        &f.venue_execution_id,
    ]
    .iter()
    .any(|v| v.len() > 4096)
    {
        return Err(Error::Capacity);
    }
    if f.fill_id.is_empty()
        || f.venue.is_empty()
        || !kanz_decimal_compat::is_positive(f.quantity.as_ref())
        || !identity::domain(f)
    {
        return Err(Error::InvalidInput);
    }
    let tx = scope(client, tenant).await?;
    tx.query_one(
        "SELECT pg_advisory_xact_lock(hashtext(app_current_tenant() || '/' || $1),hashtext($2))",
        &[&portfolio, &f.instrument_id],
    )
    .await?;
    let key = identity::execution_key(f);
    let claimed=tx.execute("INSERT INTO position_fills(fill_id,raw_fill_id) VALUES($1,$2) ON CONFLICT(tenant_id,fill_id) DO NOTHING",&[&key,&f.fill_id]).await?;
    let mut l = load(&tx, portfolio, f).await?;
    let mut price = rational(f.price.as_ref()).ok_or(Error::InvalidInput)?;
    let mut at = as_of;
    if claimed > 0 {
        let result = record_execution(&tx, portfolio, &wire, as_of, l).await?;
        l = result.0;
        price = result.1;
        at = at.max(result.2);
        let stored = [
            math::text(&l.qty),
            math::text(&l.avg),
            math::text(&l.realized),
        ];
        if stored.iter().any(|v| v.len() > 4096) {
            return Err(Error::Capacity);
        }
        tx.execute("INSERT INTO positions(portfolio_id,venue,instrument_id,quantity,average_price,realized_pnl) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(tenant_id,portfolio_id,venue,instrument_id) DO UPDATE SET quantity=EXCLUDED.quantity,average_price=EXCLUDED.average_price,realized_pnl=EXCLUDED.realized_pnl,updated_at=now()",&[&portfolio,&f.venue,&f.instrument_id,&stored[0],&stored[1],&stored[2]]).await?;
    } else {
        verify_duplicate(&tx, portfolio, &wire).await?;
        let row=tx.query_opt("SELECT execution_time,CASE WHEN octet_length(fill)<=1048576 THEN fill END FROM position_execution_history WHERE portfolio_id=$1 AND venue=$2 AND instrument_id=$3 ORDER BY execution_time DESC,execution_key DESC LIMIT 1",&[&portfolio,&f.venue,&f.instrument_id]).await?;
        if let Some(row) = row {
            let data: Vec<u8> = row
                .try_get::<_, Option<Vec<u8>>>(1)?
                .ok_or(Error::Capacity)?;
            let latest = FillWire::decode(&data)?;
            if !identity::domain(latest.value()) {
                return Err(Error::PositionHistoryUnknown);
            }
            price = rational(latest.value().price.as_ref()).ok_or(Error::PositionHistoryUnknown)?;
            at = at.max(row.try_get(0)?);
        }
    }
    let group = lots(&tx, portfolio, Some(&f.instrument_id)).await?;
    let agg = math::aggregate(group.first().map_or(&[], |(_, values)| values.as_slice()))?;
    let applied = Applied {
        venue: math::state(
            &l,
            portfolio,
            &f.venue,
            &f.instrument_id,
            &price,
            timestamp(at)?,
            ccy,
        )?,
        aggregate: math::state(
            &agg,
            portfolio,
            "",
            &f.instrument_id,
            &price,
            timestamp(at)?,
            ccy,
        )?,
    };
    if applied
        .venue
        .encoded_len()
        .checked_add(applied.aggregate.encoded_len())
        .is_none_or(|n| n > MAX_BYTES)
    {
        return Err(Error::Capacity);
    }
    let records = announce(&applied)?;
    bounded_records(&records)?;
    enqueue(&tx, &records).await?;
    commit(tx).await?;
    Ok(applied)
}
async fn record_execution(
    tx: &Transaction<'_>,
    portfolio: &str,
    wire: &FillWire,
    as_of: SystemTime,
    current: Lot,
) -> Result<(Lot, num_rational::BigRational, SystemTime), Error> {
    let f = wire.value();
    let key = identity::execution_key(f);
    let proven = f
        .executed_at
        .as_ref()
        .is_some_and(|t| identity::timestamp(t) && t.seconds > 0);
    let when = if proven {
        time(f.executed_at.as_ref().ok_or(Error::InvalidInput)?)?
    } else {
        as_of
    };
    let row=tx.query_opt("SELECT complete,last_execution_time,CASE WHEN octet_length(last_execution_key)<=1048576 THEN last_execution_key END FROM position_execution_basis WHERE portfolio_id=$1 AND venue=$2 AND instrument_id=$3",&[&portfolio,&f.venue,&f.instrument_id]).await?;
    let (mut complete, mut last_time, mut last_key) = if let Some(r) = row {
        (
            r.try_get::<_, bool>(0)?,
            r.try_get::<_, SystemTime>(1)?,
            r.try_get::<_, Option<String>>(2)?.ok_or(Error::Capacity)?,
        )
    } else {
        let exists:bool=tx.query_one("SELECT EXISTS(SELECT 1 FROM positions WHERE portfolio_id=$1 AND venue=$2 AND instrument_id=$3)",&[&portfolio,&f.venue,&f.instrument_id]).await?.try_get(0)?;
        (
            !exists,
            UNIX_EPOCH - Duration::from_secs(62135596800),
            String::new(),
        )
    };
    if !proven {
        complete = false
    }
    let backdated = when < last_time || (when == last_time && key < last_key);
    if (backdated || f.recovery.is_some()) && !complete {
        return Err(Error::PositionHistoryUnknown);
    }
    let data = wire.journal_bytes()?;
    tx.execute("INSERT INTO position_execution_history(execution_key,raw_fill_id,portfolio_id,venue,instrument_id,execution_time,time_proven,fill) VALUES($1,$2,$3,$4,$5,$6,$7,$8)",&[&key,&f.fill_id,&portfolio,&f.venue,&f.instrument_id,&when,&proven,&data]).await?;
    if f.recovery
        .as_ref()
        .and_then(|r| r.fee_approval.as_ref())
        .is_some()
    {
        let previous = fee_revision::terms(wire)?;
        fee_journal::record_for(tx, Book::Position, &previous, wire).await?;
    }
    let mut l = current;
    let mut price = rational(f.price.as_ref()).ok_or(Error::InvalidInput)?;
    if backdated {
        let rows=tx.query("SELECT CASE WHEN n<=10000 AND bytes<=1048576 THEN fill END,time_proven FROM (SELECT fill,time_proven,execution_time,execution_key,count(*) OVER() n,sum(octet_length(fill)::bigint) OVER() bytes FROM position_execution_history WHERE portfolio_id=$1 AND venue=$2 AND instrument_id=$3) bounded ORDER BY execution_time,execution_key LIMIT 10001",&[&portfolio,&f.venue,&f.instrument_id]).await?;
        if rows.len() > MAX_ROWS {
            return Err(Error::PositionHistoryUnknown);
        }
        l = Lot::default();
        for row in rows {
            if !row.try_get::<_, bool>(1)? {
                return Err(Error::PositionHistoryUnknown);
            }
            let data = row
                .try_get::<_, Option<Vec<u8>>>(0)?
                .ok_or(Error::Capacity)?;
            let execution = FillWire::decode(&data)?;
            if !identity::domain(execution.value()) {
                return Err(Error::PositionHistoryUnknown);
            }
            l.fold(execution.value())?;
            price =
                rational(execution.value().price.as_ref()).ok_or(Error::PositionHistoryUnknown)?;
        }
    } else {
        l.fold(f)?;
        last_time = when;
        last_key = key;
    }
    tx.execute("INSERT INTO position_execution_basis(portfolio_id,venue,instrument_id,complete,last_execution_time,last_execution_key) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(tenant_id,portfolio_id,venue,instrument_id) DO UPDATE SET complete=EXCLUDED.complete,last_execution_time=EXCLUDED.last_execution_time,last_execution_key=EXCLUDED.last_execution_key",&[&portfolio,&f.venue,&f.instrument_id,&complete,&last_time,&last_key]).await?;
    Ok((l, price, last_time))
}
async fn verify_duplicate(
    tx: &Transaction<'_>,
    portfolio: &str,
    revised: &FillWire,
) -> Result<(), Error> {
    let f = revised.value();
    let key = identity::execution_key(f);
    let row=tx.query_opt("SELECT CASE WHEN octet_length(portfolio_id)<=4096 THEN portfolio_id END,CASE WHEN octet_length(fill)<=1048576 THEN fill END FROM position_execution_history WHERE execution_key=$1",&[&key]).await?;
    let Some(row) = row else {
        if f.recovery.is_some() {
            return Err(Error::PositionHistoryUnknown);
        }
        return Ok(());
    };
    let p: String = row
        .try_get::<_, Option<String>>(0)?
        .ok_or(Error::Capacity)?;
    if p != portfolio {
        return Err(Error::IdentityConflict);
    }
    let raw: Vec<u8> = row
        .try_get::<_, Option<Vec<u8>>>(1)?
        .ok_or(Error::Capacity)?;
    let original = FillWire::decode(&raw)?;
    if f.recovery
        .as_ref()
        .and_then(|r| r.fee_approval.as_ref())
        .is_some()
    {
        fee_journal::record_for(tx, Book::Position, &original, revised).await?;
        return Ok(());
    }
    let head = fee_journal::head_for(tx, Book::Position, &original).await?;
    if !(identity::same_execution(head.value(), f)
        || f.recovery.is_none() && identity::same_execution(original.value(), f))
    {
        return Err(Error::IdentityConflict);
    }
    Ok(())
}
/// Cost-valued legacy snapshot, NOT live valuation or a genesis checkpoint.
/// Cash and log_position remain absent exactly as in the Go owner.
pub async fn snapshot(
    client: &mut Client,
    tenant: &str,
    portfolio: &str,
    as_of: SystemTime,
    base_currency: &str,
) -> Result<PortfolioSnapshot, Error> {
    if portfolio.len() > 4096 {
        return Err(Error::Capacity);
    }
    let ccy = currency(base_currency)?;
    let at = timestamp(as_of)?;
    let tx = scope(client, tenant).await?;
    let groups = lots(&tx, portfolio, None).await?;
    let mut positions = Vec::new();
    let mut nav = num_rational::BigRational::zero();
    for (instrument, group) in groups {
        let l = math::aggregate(&group)?;
        nav += &l.qty * &l.avg;
        math::bound(&nav)?;
        let mut st = math::state(&l, portfolio, "", &instrument, &l.avg, at, ccy)?;
        st.unrealized_pnl = None;
        positions.push(st);
    }
    let result = PortfolioSnapshot {
        portfolio: Some(PortfolioState {
            portfolio_id: portfolio.into(),
            base_currency: ccy.into(),
            total_market_value: Some(math::money(nav, ccy)?),
            position_count: positions.len().try_into().map_err(|_| Error::Capacity)?,
            as_of: Some(at),
            ..Default::default()
        }),
        positions,
        log_position: None,
    };
    if result.encoded_len() > MAX_BYTES {
        return Err(Error::Capacity);
    }
    tx.rollback().await?;
    Ok(result)
}

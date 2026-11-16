//! Existing OMS proposal persistence, not maker/checker authentication or admission.
use super::{enqueue, fee_subject, scoped, Error, Record, MAX_BYTES};
use kanz_order_core::proto::order::v1::SubmitOrder;
use prost::Message;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio_postgres::{Client, Row, Transaction};

pub const MAX_PROPOSALS: usize = 4096;

pub struct Proposal {
    pub id: String,
    pub subject: String,
    pub portfolio_id: String,
    pub act: String,
    pub proposer: String,
    pub digest: String,
    pub command: Vec<u8>,
    pub created_at: SystemTime,
    pub expires_at: SystemTime,
    pub approver: String,
    pub decided_at: Option<SystemTime>,
    pub refusal_reason: String,
    pub refused_by: String,
    pub refused_at: Option<SystemTime>,
    pub expiry_announced_at: Option<SystemTime>,
}

fn blank(value: &str) -> bool {
    fee_subject::normalize(value).is_empty()
}
fn zero(value: SystemTime) -> bool {
    UNIX_EPOCH.checked_sub(Duration::from_secs(62135596800)) == Some(value)
}
fn budget(command: &[u8], fields: &[&str], records: &[Record]) -> Result<(), Error> {
    let mut size = command.len();
    if records.len() > 64 {
        return Err(Error::Capacity);
    }
    for field in fields {
        size = size.checked_add(field.len()).ok_or(Error::Capacity)?;
    }
    for record in records {
        size = size
            .checked_add(record.payload.len())
            .ok_or(Error::Capacity)?;
        for field in [
            &record.tenant,
            &record.partition_key,
            &record.subject,
            &record.event_type,
            &record.domain,
            &record.payload_schema_ref,
            &record.correlation_id,
            &record.causation_id,
            &record.trace_context,
        ] {
            size = size.checked_add(field.len()).ok_or(Error::Capacity)?;
        }
    }
    if size > MAX_BYTES {
        return Err(Error::Capacity);
    }
    Ok(())
}
async fn proposal_scope<'a>(
    client: &'a mut Client,
    tenant: &str,
) -> Result<Transaction<'a>, Error> {
    let tx = scoped(client, tenant).await?;
    let safe:bool=tx.query_one("SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid='order_proposals'::regclass",&[]).await?.try_get(0)?;
    if !safe {
        return Err(Error::UnsafeRole);
    }
    Ok(tx)
}

/// Like Go Put, decision/refusal fields on the input are not inserted.
pub async fn put(
    client: &mut Client,
    tenant: &str,
    p: &Proposal,
    records: &[Record],
) -> Result<(), Error> {
    budget(
        &p.command,
        &[
            &p.id,
            &p.subject,
            &p.portfolio_id,
            &p.act,
            &p.proposer,
            &p.digest,
        ],
        records,
    )?;
    if blank(&p.id)
        || p.act.is_empty()
        || blank(&p.subject)
        || blank(&p.proposer)
        || blank(&p.digest)
        || zero(p.created_at)
        || zero(p.expires_at)
        || p.expires_at <= p.created_at
        || p.id != p.subject
    {
        return Err(Error::InvalidInput);
    }
    let command = SubmitOrder::decode(p.command.as_slice()).map_err(|_| Error::InvalidInput)?;
    if command.order_id != p.id {
        return Err(Error::InvalidInput);
    }
    let tx = proposal_scope(client, tenant).await?;
    let changed=tx.execute("INSERT INTO order_proposals (tenant_id,order_id,portfolio_id,act,proposer,digest,command,created_at,expires_at) VALUES(app_current_tenant(),$1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(tenant_id,order_id) DO NOTHING",&[&p.id,&p.portfolio_id,&p.act,&p.proposer,&p.digest,&p.command,&p.created_at,&p.expires_at]).await?;
    if changed != 1 {
        return Err(Error::Exists);
    }
    enqueue(&tx, records).await?;
    tx.commit().await.map_err(|_| Error::CommitUnconfirmed)
}

// Dynamic fields are withheld by SQL if the entire result exceeds the byte cap.
const COLUMNS:&str="CASE WHEN fits THEN ARRAY[order_id,portfolio_id,act,proposer,digest,approver,refusal_reason,refused_by] END,CASE WHEN fits THEN command END,decided_at,created_at,expires_at,refused_at,expiry_announced_at";
const BOUND:&str="sum(octet_length(order_id)+octet_length(portfolio_id)+octet_length(act)+octet_length(proposer)+octet_length(digest)+octet_length(approver)+octet_length(refusal_reason)+octet_length(refused_by)+octet_length(command)) OVER ()<=1048576";
fn decode(rows: Vec<Row>) -> Result<Vec<Proposal>, Error> {
    if rows.len() > MAX_PROPOSALS {
        return Err(Error::Capacity);
    }
    let mut out = Vec::with_capacity(rows.len());
    for row in rows {
        let fields = row
            .try_get::<_, Option<Vec<String>>>(0)?
            .ok_or(Error::Capacity)?;
        let mut fields = fields.into_iter();
        let mut next = || fields.next().ok_or(Error::Database);
        let id = next()?;
        let command = row
            .try_get::<_, Option<Vec<u8>>>(1)?
            .ok_or(Error::Capacity)?;
        SubmitOrder::decode(command.as_slice()).map_err(|_| Error::InvalidInput)?;
        out.push(Proposal {
            subject: id.clone(),
            id,
            portfolio_id: next()?,
            act: next()?,
            proposer: next()?,
            digest: next()?,
            approver: next()?,
            refusal_reason: next()?,
            refused_by: next()?,
            command,
            decided_at: row.try_get(2)?,
            created_at: row.try_get(3)?,
            expires_at: row.try_get(4)?,
            refused_at: row.try_get(5)?,
            expiry_announced_at: row.try_get(6)?,
        });
    }
    Ok(out)
}
pub async fn get(client: &mut Client, tenant: &str, id: &str) -> Result<Option<Proposal>, Error> {
    budget(&[], &[id], &[])?;
    let tx = proposal_scope(client, tenant).await?;
    let sql = format!(
        "SELECT {COLUMNS} FROM (SELECT *,{BOUND} AS fits FROM order_proposals WHERE order_id=$1) p"
    );
    let mut rows = decode(tx.query(&sql, &[&id]).await?)?;
    tx.rollback().await?;
    Ok(rows.pop())
}
pub async fn pending(
    client: &mut Client,
    tenant: &str,
    now: SystemTime,
) -> Result<Vec<Proposal>, Error> {
    let tx = proposal_scope(client, tenant).await?;
    let sql=format!("SELECT {COLUMNS} FROM (SELECT *,{BOUND} AS fits FROM (SELECT * FROM order_proposals WHERE approver='' AND expiry_announced_at IS NULL AND expires_at>$1 ORDER BY created_at,order_id LIMIT 4097) selected) p ORDER BY created_at,order_id");
    let rows = decode(tx.query(&sql, &[&now]).await?)?;
    tx.rollback().await?;
    Ok(rows)
}
pub async fn expired_unannounced(
    client: &mut Client,
    tenant: &str,
    now: SystemTime,
    limit: i64,
) -> Result<Vec<Proposal>, Error> {
    let limit = if limit <= 0 { 100 } else { limit };
    if limit > MAX_PROPOSALS as i64 {
        return Err(Error::Capacity);
    }
    let tx = proposal_scope(client, tenant).await?;
    let sql=format!("SELECT {COLUMNS} FROM (SELECT *,{BOUND} AS fits FROM (SELECT * FROM order_proposals WHERE approver='' AND expiry_announced_at IS NULL AND expires_at<=$1 ORDER BY expires_at,order_id LIMIT $2) selected) p ORDER BY expires_at,order_id");
    let rows = decode(tx.query(&sql, &[&now, &limit]).await?)?;
    tx.rollback().await?;
    Ok(rows)
}

/// The caller must already have applied canonical dualcontrol.Approve. Like Go,
/// this repository arbitrates the claim; it does not invent a DB-clock policy.
pub async fn claim(
    client: &mut Client,
    tenant: &str,
    id: &str,
    approver: &str,
    at: SystemTime,
    records: &[Record],
) -> Result<bool, Error> {
    budget(&[], &[id, approver], records)?;
    if blank(approver) || records.is_empty() {
        return Err(Error::InvalidInput);
    }
    let tx = proposal_scope(client, tenant).await?;
    let changed=tx.execute("UPDATE order_proposals SET approver=$2,decided_at=$3 WHERE order_id=$1 AND approver='' AND expiry_announced_at IS NULL AND lower(btrim(proposer))<>lower(btrim($2))",&[&id,&approver,&at]).await?;
    if changed != 1 {
        tx.rollback().await?;
        return Ok(false);
    }
    enqueue(&tx, records).await?;
    tx.commit().await.map_err(|_| Error::CommitUnconfirmed)?;
    Ok(true)
}
pub async fn record_refusal(
    client: &mut Client,
    tenant: &str,
    id: &str,
    approver: &str,
    reason: &str,
    at: SystemTime,
) -> Result<bool, Error> {
    budget(&[], &[id, approver, reason], &[])?;
    if blank(approver) || blank(reason) || zero(at) {
        return Err(Error::InvalidInput);
    }
    let tx = proposal_scope(client, tenant).await?;
    let changed=tx.execute("UPDATE order_proposals SET refusal_reason=$3,refused_by=$2,refused_at=$4 WHERE order_id=$1 AND approver=''",&[&id,&approver,&reason,&at]).await?;
    tx.commit().await.map_err(|_| Error::CommitUnconfirmed)?;
    Ok(changed == 1)
}
pub async fn announce_expiry(
    client: &mut Client,
    tenant: &str,
    id: &str,
    at: SystemTime,
    records: &[Record],
) -> Result<bool, Error> {
    budget(&[], &[id], records)?;
    let tx = proposal_scope(client, tenant).await?;
    let changed=tx.execute("UPDATE order_proposals SET expiry_announced_at=$2 WHERE order_id=$1 AND approver='' AND expiry_announced_at IS NULL AND expires_at<=$2",&[&id,&at]).await?;
    if changed == 0 {
        tx.rollback().await?;
        return Ok(false);
    }
    enqueue(&tx, records).await?;
    tx.commit().await.map_err(|_| Error::CommitUnconfirmed)?;
    Ok(true)
}

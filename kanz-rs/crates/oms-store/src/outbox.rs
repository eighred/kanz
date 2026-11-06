//! OMS-owned queue persistence. No broker publication or admission authority.
use super::{scoped, Error, Record, MAX_BYTES};
use std::time::{Duration, SystemTime};
use std::{future::Future, pin::Pin};
use tokio_postgres::{Client, Transaction};

pub const MAX_PAGE: i64 = 4096;
const SUFFIX: &str = "/oms/outbox";
const LOCK_IDS: &str = "SELECT hashtext(current_database() || '/' || ('outbox'::regclass)::oid::text || '/' || app_current_tenant() || $2),hashtext($1)";

pub struct Pending {
    pub id: i64,
    pub attempts: i32,
    pub record: Record,
    pub last_error: String,
}

async fn owned(tx: &Transaction<'_>) -> Result<(), Error> {
    if tx.query_one("SELECT EXISTS(SELECT 1 FROM outbox WHERE published_at IS NULL AND producer_service IS NULL)", &[]).await?.try_get::<_,bool>(0)? {
        return Err(Error::UnownedOutbox);
    }
    Ok(())
}

fn page(limit: i64) -> Result<(), Error> {
    if limit > MAX_PAGE {
        return Err(Error::Capacity);
    }
    Ok(())
}

pub async fn pending_keys(
    client: &mut Client,
    tenant: &str,
    limit: i64,
) -> Result<Vec<String>, Error> {
    if limit <= 0 {
        return Ok(Vec::new());
    }
    page(limit)?;
    let tx = scoped(client, tenant).await?;
    owned(&tx).await?;
    // Bound bytes in SQL before transferring any unrestricted TEXT to the client.
    let rows = tx.query("SELECT CASE WHEN sum(octet_length(partition_key)) OVER () <= $2 THEN partition_key END FROM (SELECT partition_key,min(id) AS first_id FROM outbox WHERE published_at IS NULL AND producer_service='oms' GROUP BY partition_key ORDER BY min(id) LIMIT $1) p ORDER BY first_id", &[&limit,&(MAX_BYTES as i64)]).await?;
    let keys = rows
        .iter()
        .map(|r| r.try_get::<_, Option<String>>(0)?.ok_or(Error::Capacity))
        .collect::<Result<Vec<_>, Error>>()?;
    tx.rollback().await?;
    Ok(keys)
}

pub async fn pending(
    client: &mut Client,
    tenant: &str,
    key: &str,
    limit: i64,
) -> Result<Vec<Pending>, Error> {
    if limit <= 0 {
        return Ok(Vec::new());
    }
    page(limit)?;
    if key.len() > MAX_BYTES {
        return Err(Error::Capacity);
    }
    let tx = scoped(client, tenant).await?;
    owned(&tx).await?;
    // Each dynamic field is withheld when the whole requested page exceeds the
    // byte budget. A failed page never returns an apparently complete prefix.
    let rows = tx.query("WITH selected AS (SELECT * FROM outbox WHERE published_at IS NULL AND partition_key=$1 AND producer_service='oms' ORDER BY id LIMIT $2), bounded AS (SELECT *,sum(octet_length(envelope_tenant_id)+octet_length(partition_key)+octet_length(subject)+octet_length(event_type)+octet_length(domain)+octet_length(payload_schema_ref)+octet_length(correlation_id)+octet_length(causation_id)+octet_length(trace_context)+octet_length(payload)+octet_length(last_error)) OVER () <= $3 AS fits FROM selected) SELECT id,attempts,event_class,schema_version,event_time,CASE WHEN fits THEN ARRAY[envelope_tenant_id,partition_key,subject,event_type,domain,payload_schema_ref,correlation_id,causation_id,trace_context,last_error] END,CASE WHEN fits THEN payload END FROM bounded ORDER BY id", &[&key,&limit,&(MAX_BYTES as i64)]).await?;
    let mut result = Vec::with_capacity(rows.len());
    for row in rows {
        let fields = row
            .try_get::<_, Option<Vec<String>>>(5)?
            .ok_or(Error::Capacity)?;
        let mut fields = fields.into_iter();
        let mut next = || fields.next().ok_or(Error::Database);
        let record = Record {
            tenant: next()?,
            partition_key: next()?,
            subject: next()?,
            event_type: next()?,
            domain: next()?,
            payload_schema_ref: next()?,
            correlation_id: next()?,
            causation_id: next()?,
            trace_context: next()?,
            event_class: row.try_get(2)?,
            schema_version: row.try_get(3)?,
            event_time: row.try_get(4)?,
            payload: row
                .try_get::<_, Option<Vec<u8>>>(6)?
                .ok_or(Error::Capacity)?,
        };
        result.push(Pending {
            id: row.try_get(0)?,
            attempts: row.try_get(1)?,
            record,
            last_error: next()?,
        });
    }
    tx.rollback().await?;
    Ok(result)
}

pub async fn mark_published(client: &mut Client, tenant: &str, id: i64) -> Result<(), Error> {
    let tx = scoped(client, tenant).await?;
    if tx.execute("UPDATE outbox SET published_at=COALESCE(published_at,now()),last_error='' WHERE id=$1 AND producer_service='oms'", &[&id]).await? == 0 { return Err(Error::RecordNotOwned); }
    tx.commit().await.map_err(|_| Error::CommitUnconfirmed)
}

fn cause(message: Option<&str>) -> Result<&str, Error> {
    let message = message.unwrap_or("");
    // Go truncates bytes, not Unicode scalars. PostgreSQL refuses a cut UTF-8
    // codepoint; explicitly refuse the same input rather than silently repair it.
    std::str::from_utf8(&message.as_bytes()[..message.len().min(1000)])
        .map_err(|_| Error::InvalidInput)
}

pub async fn mark_failed(
    client: &mut Client,
    tenant: &str,
    id: i64,
    message: Option<&str>,
) -> Result<(), Error> {
    let message = cause(message)?;
    let tx = scoped(client, tenant).await?;
    let owned: bool = tx.query_one("WITH owned AS (SELECT id FROM outbox WHERE id=$1 AND producer_service='oms'), updated AS (UPDATE outbox SET attempts=attempts+1,last_error=$2 WHERE id IN (SELECT id FROM owned) AND published_at IS NULL RETURNING id) SELECT EXISTS(SELECT 1 FROM owned)", &[&id,&message]).await?.try_get(0)?;
    if !owned {
        return Err(Error::RecordNotOwned);
    }
    tx.commit().await.map_err(|_| Error::CommitUnconfirmed)
}

pub async fn oldest_pending_age(
    client: &mut Client,
    tenant: &str,
    now: SystemTime,
) -> Result<Option<Duration>, Error> {
    let tx = scoped(client, tenant).await?;
    owned(&tx).await?;
    let oldest: Option<SystemTime> = tx.query_one("SELECT min(enqueued_at) FROM outbox WHERE published_at IS NULL AND producer_service='oms'", &[]).await?.try_get(0)?;
    tx.rollback().await?;
    Ok(oldest.map(|t| {
        now.duration_since(t)
            .unwrap_or_default()
            .min(Duration::from_nanos(i64::MAX as u64))
    }))
}

/// Dedicated driver ownership is mandatory: dropping Client alone can leave a
/// queued advisory-lock wait running. This session cannot return to any pool.
type Cancellation = Pin<Box<dyn Future<Output = Result<(), tokio_postgres::Error>> + Send>>;

pub struct Session {
    client: Client,
    driver: Option<tokio::task::JoinHandle<bool>>,
    cancellation: Option<Cancellation>,
}
impl Session {
    /// The trusted connector must supply this client's own running driver task,
    /// with no independently retained client handle or replacement driver.
    /// The connector supplies its existing TLS policy for PostgreSQL's separate
    /// CancelRequest connection; this owner does not choose transport or secrets.
    pub fn new<F, C>(client: Client, driver: tokio::task::JoinHandle<bool>, cancel: F) -> Self
    where
        F: FnOnce(tokio_postgres::CancelToken) -> C,
        C: Future<Output = Result<(), tokio_postgres::Error>> + Send + 'static,
    {
        let cancellation = Some(Box::pin(cancel(client.cancel_token())) as Cancellation);
        Self {
            client,
            driver: Some(driver),
            cancellation,
        }
    }
}
impl Drop for Session {
    fn drop(&mut self) {
        if let Some(driver) = self.driver.take() {
            // Socket close alone does not interrupt a PostgreSQL backend inside
            // an advisory-lock wait. Dispatch protocol cancellation while the
            // runtime is alive; server lock_timeout is the shutdown fallback.
            if let (Ok(runtime), Some(cancel)) = (
                tokio::runtime::Handle::try_current(),
                self.cancellation.take(),
            ) {
                runtime.spawn(async move {
                    let _ = tokio::time::timeout(Duration::from_secs(5), cancel).await;
                    driver.abort();
                });
            } else {
                driver.abort();
            }
        }
    }
}

pub struct DrainLock {
    session: Session,
    scope: i32,
    key: i32,
}
impl DrainLock {
    /// Only a verified unlock returns the session. Timeout, error or cancellation
    /// drops it and aborts its driver, including a possibly still-held lock.
    pub async fn release(self) -> Result<Session, Error> {
        let unlocked: bool = tokio::time::timeout(
            Duration::from_secs(5),
            self.session.client.query_one(
                "SELECT pg_advisory_unlock($1,$2)",
                &[&self.scope, &self.key],
            ),
        )
        .await
        .map_err(|_| Error::Database)??
        .try_get(0)?;
        if !unlocked {
            return Err(Error::Database);
        }
        Ok(self.session)
    }
}

/// None means contention (try mode). Dropping this future closes the dedicated
/// session; the caller controls a deadline within the five-second server ceiling.
/// Session-level, not xact,
/// locking preserves the Go relay's lock lifetime and exact hash namespace.
pub async fn lock_key(
    mut session: Session,
    tenant: &str,
    key: &str,
    wait: bool,
) -> Result<Option<DrainLock>, Error> {
    if key.len() > MAX_BYTES {
        return Err(Error::Capacity);
    }
    let tx = scoped(&mut session.client, tenant).await?;
    // A failed CancelRequest or a runtime already shutting down cannot leave a
    // blocked backend indefinitely. This bounded port refuses waits above5s.
    tx.batch_execute("SET LOCAL lock_timeout='5s'").await?;
    let row = tx.query_one(LOCK_IDS, &[&key, &SUFFIX]).await?;
    let scope: i32 = row.try_get(0)?;
    let key: i32 = row.try_get(1)?;
    let won = if wait {
        tx.query_one("SELECT pg_advisory_lock($1,$2)", &[&scope, &key])
            .await?;
        true
    } else {
        tx.query_one("SELECT pg_try_advisory_lock($1,$2)", &[&scope, &key])
            .await?
            .try_get(0)?
    };
    tx.rollback().await?;
    Ok(won.then_some(DrainLock {
        session,
        scope,
        key,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn cause_matches_go_byte_bound() {
        assert_eq!(cause(None), Ok(""));
        assert_eq!(cause(Some(&"x".repeat(1001))).unwrap().len(), 1000);
        assert_eq!(
            cause(Some(&format!("{}é", "x".repeat(999)))),
            Err(Error::InvalidInput)
        );
    }
}

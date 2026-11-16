//! Bounded canonical repository reads. Ordering matches Go; no cross-query snapshot promise.
use super::{scoped, Error, OrderState, MAX_BYTES};
use prost::Message;
use tokio_postgres::{Client, Row};

const MAX_ROWS: usize = 100000;

fn states(rows: Vec<Row>) -> Result<Vec<Vec<u8>>, Error> {
    if rows.len() > MAX_ROWS {
        return Err(Error::Capacity);
    }
    let mut total = 0usize;
    let mut result = Vec::with_capacity(rows.len());
    for row in rows {
        let raw = row
            .try_get::<_, Option<Vec<u8>>>(0)?
            .ok_or(Error::Capacity)?;
        total = total.checked_add(raw.len()).ok_or(Error::Capacity)?;
        if total > MAX_BYTES {
            return Err(Error::Capacity);
        }
        OrderState::decode(raw.as_slice()).map_err(|_| Error::InvalidInput)?;
        result.push(raw);
    }
    Ok(result)
}

pub async fn list(client: &mut Client, tenant: &str) -> Result<Vec<Vec<u8>>, Error> {
    let tx = scoped(client, tenant).await?;
    let rows = tx.query("SELECT CASE WHEN sum(octet_length(state)) OVER ()<=$1 THEN state END FROM (SELECT order_id,state FROM orders ORDER BY order_id LIMIT 100001) selected ORDER BY order_id", &[&(MAX_BYTES as i64)]).await?;
    let result = states(rows)?;
    tx.rollback().await?;
    Ok(result)
}

pub async fn list_by_status(
    client: &mut Client,
    tenant: &str,
    statuses: &[i32],
) -> Result<Vec<Vec<u8>>, Error> {
    if statuses.len() > 128 {
        return Err(Error::Capacity);
    }
    let tx = scoped(client, tenant).await?;
    // Unknown numeric statuses remain valid selectors, as in the canonical Go store.
    let rows = tx.query("SELECT CASE WHEN sum(octet_length(state)) OVER ()<=$2 THEN state END FROM (SELECT order_id,state FROM orders WHERE status=ANY($1) ORDER BY order_id LIMIT 100001) selected ORDER BY order_id", &[&statuses, &(MAX_BYTES as i64)]).await?;
    let result = states(rows)?;
    tx.rollback().await?;
    Ok(result)
}

pub async fn list_by_parent(
    client: &mut Client,
    tenant: &str,
    parent: &str,
) -> Result<Vec<Vec<u8>>, Error> {
    if parent.len() > MAX_BYTES {
        return Err(Error::Capacity);
    }
    let tx = scoped(client, tenant).await?;
    let rows = tx.query("SELECT CASE WHEN sum(octet_length(state)) OVER ()<=$2 THEN state END FROM (SELECT order_id,state FROM orders WHERE parent_order_id=$1 AND parent_order_id<>'' ORDER BY order_id LIMIT 100001) selected ORDER BY order_id", &[&parent, &(MAX_BYTES as i64)]).await?;
    let result = states(rows)?;
    tx.rollback().await?;
    Ok(result)
}

pub struct PortfolioPage {
    pub states: Vec<Vec<u8>>,
    pub unindexed: i64,
}

pub async fn list_by_portfolio(
    client: &mut Client,
    tenant: &str,
    portfolio: &str,
    limit: i64,
) -> Result<PortfolioPage, Error> {
    if portfolio.len() > MAX_BYTES {
        return Err(Error::Capacity);
    }
    let tx = scoped(client, tenant).await?;
    if portfolio.is_empty() {
        tx.rollback().await?;
        return Ok(PortfolioPage {
            states: Vec::new(),
            unindexed: 0,
        });
    }
    let limit = if limit <= 0 || limit > 200 {
        200
    } else {
        limit
    };
    let rows = tx.query("SELECT CASE WHEN sum(octet_length(state)) OVER ()<=$3 THEN state END FROM (SELECT created_at,state FROM orders WHERE portfolio_id=$1 ORDER BY created_at DESC LIMIT $2) selected ORDER BY created_at DESC", &[&portfolio, &limit, &(MAX_BYTES as i64)]).await?;
    let result = states(rows)?;
    // Deliberately separate READ COMMITTED statement, matching Go. Concurrent
    // writes may change this count between the page and count observations.
    let unindexed: i64 = tx
        .query_one("SELECT count(*) FROM orders WHERE portfolio_id=''", &[])
        .await?
        .try_get(0)?;
    tx.rollback().await?;
    Ok(PortfolioPage {
        states: result,
        unindexed,
    })
}

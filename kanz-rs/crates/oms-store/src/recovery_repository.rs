//! Immutable observation/mapping/history owners used by recovery coordination.
//! Mapping provenance and complete-history preparation remain the coordinator's
//! typed responsibilities; these APIs never grant financial admission.
use super::{
    commit, enqueue, identity, recovery, scoped, validate_batch, Error, Record, MAX_BYTES,
};
use kanz_order_core::proto::order::v1::{
    ExecutionRecoveryHistory, ExecutionRecoveryRecorded, OrderState,
};
use kanz_proto_wire::{Budget, Canonical};
use prost::Message;
use sha2::{Digest, Sha256};
use std::collections::BTreeSet;
use tokio_postgres::{types::Type, Client, Transaction};

/// Bounded one-row cursor over a single read snapshot. It neither retains all
/// raw journal bytes nor truncates a page and calls that complete history.
pub struct PostedReader<'a> {
    tx: Transaction<'a>,
    order: String,
    after: Option<String>,
    remaining: usize,
    seen: BTreeSet<String>,
}
pub struct PostedExecution {
    pub original: super::fee_wire::FillWire,
    pub current: super::fee_wire::FillWire,
}
pub async fn posted_reader<'a>(
    client: &'a mut Client,
    tenant: &str,
    order: &str,
) -> Result<PostedReader<'a>, Error> {
    if !text(order, 256) {
        return Err(Error::InvalidInput);
    }
    let tx = client
        .build_transaction()
        .isolation_level(tokio_postgres::IsolationLevel::RepeatableRead)
        .read_only(true)
        .start()
        .await?;
    super::scope_existing(&tx, tenant).await?;
    let row=tx.query_one("SELECT (SELECT count(*) FROM order_executions WHERE order_id=$1),(SELECT count(*) FROM order_fills WHERE order_id=$1),(SELECT count(*) FROM (SELECT DISTINCT execution_key FROM execution_fee_revisions WHERE book='oms' AND order_id=$1 LIMIT 8001) heads)",&[&order]).await?;
    let rows: i64 = row.get(0);
    let claims: i64 = row.get(1);
    let heads: i64 = row.get(2);
    if rows != claims || rows > MAX_FILLS as i64 || heads > MAX_FILLS as i64 {
        return Err(Error::PostedExecutionUnknown);
    }
    Ok(PostedReader {
        tx,
        order: order.into(),
        after: None,
        remaining: rows as usize,
        seen: BTreeSet::new(),
    })
}
fn posted_key(f: &kanz_order_core::proto::order::v1::Fill) -> Result<String, Error> {
    // Validate before execution_key can fall back to cloning a malformed ID.
    if f.fill_id.len() > 1024
        || f.venue.is_empty()
        || f.venue_account_id.is_empty()
        || f.instrument_id.is_empty()
        || f.venue_execution_id.is_empty()
    {
        return Err(Error::PostedExecutionUnknown);
    }
    let key = identity::execution_key(f);
    if key.len() > 256 {
        return Err(Error::Capacity);
    }
    Ok(key)
}
impl PostedReader<'_> {
    pub async fn next(&mut self) -> Result<Option<PostedExecution>, Error> {
        if self.remaining == 0 {
            return Ok(None);
        }
        let limit = MAX_BYTES as i64;
        let row=self.tx.query_opt("SELECT CASE WHEN octet_length(fill_id)<=1024 THEN fill_id END,CASE WHEN octet_length(fill)::bigint<=$3 THEN fill END FROM order_executions WHERE order_id=$1 AND ($2::text IS NULL OR fill_id>$2) ORDER BY fill_id LIMIT 1",&[&self.order,&self.after,&limit]).await?.ok_or(Error::PostedExecutionUnknown)?;
        let id: String = row.get::<_, Option<String>>(0).ok_or(Error::Capacity)?;
        let raw: Vec<u8> = row.get::<_, Option<Vec<u8>>>(1).ok_or(Error::Capacity)?;
        let original = super::fee_wire::FillWire::decode(&raw)?;
        let key = posted_key(original.value())?;
        if !self.seen.insert(key) {
            return Err(Error::PostedExecutionUnknown);
        }
        let current = super::fee_journal::head(&self.tx, &original).await?;
        self.after = Some(id);
        self.remaining -= 1;
        Ok(Some(PostedExecution { original, current }))
    }
    pub async fn finish(self) -> Result<(), Error> {
        if self.remaining != 0 {
            return Err(Error::PostedExecutionUnknown);
        }
        let rows=self.tx.query("SELECT CASE WHEN octet_length(execution_key)<=256 THEN execution_key END FROM (SELECT DISTINCT execution_key FROM execution_fee_revisions WHERE book='oms' AND order_id=$1 LIMIT 8001) heads",&[&self.order]).await?;
        if rows.len() > MAX_FILLS {
            return Err(Error::Capacity);
        }
        for row in rows {
            let key: String = row.get::<_, Option<String>>(0).ok_or(Error::Capacity)?;
            if !self.seen.contains(&key) {
                return Err(Error::FeeRevision);
            }
        }
        commit(self.tx).await
    }
}

pub const HISTORY_BYTES: usize = 4 * MAX_BYTES;
pub const MAX_FILLS: usize = 8000;

/// Immutable typed approval bytes, not a permit. The coordinator revalidates
/// proposal digest, maker/checker, expiry, history and current fee baseline.
pub struct FeeEvidence {
    pub proposal: Vec<u8>,
    pub stored_digest: String,
    pub approval: Vec<u8>,
}
pub async fn fee_evidence(
    client: &mut Client,
    tenant: &str,
    case: &str,
) -> Result<Option<FeeEvidence>, Error> {
    let tx = scope(client, tenant).await?;
    let limit = HISTORY_BYTES as i64;
    let row=tx.query_opt("SELECT CASE WHEN octet_length(a.approval)::bigint<=$2 THEN a.approval END,CASE WHEN octet_length(p.proposal)::bigint<=$2 THEN p.proposal END,CASE WHEN octet_length(p.digest)<=64 THEN p.digest END FROM execution_fee_approvals a LEFT JOIN execution_fee_proposals p USING (tenant_id,case_id) WHERE a.case_id=$1",&[&case,&limit]).await?;
    let evidence = match row {
        None => None,
        Some(row) => Some(FeeEvidence {
            approval: row.get::<_, Option<Vec<u8>>>(0).ok_or(Error::Capacity)?,
            proposal: row.get::<_, Option<Vec<u8>>>(1).ok_or(Error::FeeRevision)?,
            stored_digest: row.get::<_, Option<String>>(2).ok_or(Error::FeeRevision)?,
        }),
    };
    commit(tx).await?;
    Ok(evidence)
}
#[derive(Clone)]
pub struct Case {
    pub id: String,
    pub order_id: String,
    pub mapping_version: String,
    pub source_cursor: String,
    pub digest: String,
    pub evidence: Vec<u8>,
    pub status: String,
    pub reason: String,
    pub checkpoint: i32,
    pub version: i64,
}
pub fn text(s: &str, bound: usize) -> bool {
    !s.is_empty() && s.len() <= bound && s.trim() == s && !s.contains('\0')
}
pub fn digest(raw: &[u8]) -> String {
    format!("{:x}", Sha256::digest(raw))
}
pub struct Mapping<'a> {
    pub tenant: &'a str,
    pub portfolio: &'a str,
    pub venue: &'a str,
    pub account: &'a str,
    pub exchange_account: &'a str,
}
impl Mapping<'_> {
    pub fn version(&self) -> Result<String, Error> {
        let fields = [
            ("Tenant", self.tenant),
            ("Portfolio", self.portfolio),
            ("Venue", self.venue),
            ("Account", self.account),
            ("ExchangeAccount", self.exchange_account),
        ];
        let mut json = String::from("{");
        for (i, (key, value)) in fields.into_iter().enumerate() {
            if !text(value, 256) {
                return Err(Error::InvalidInput);
            }
            if i != 0 {
                json.push(',');
            }
            identity::quoted(key, &mut json);
            json.push(':');
            identity::quoted(value, &mut json);
        }
        json.push('}');
        Ok(digest(json.as_bytes()))
    }
}
async fn scope<'a>(client: &'a mut Client, tenant: &str) -> Result<Transaction<'a>, Error> {
    let tx = scoped(client, tenant).await?;
    let n:i64=tx.query_typed_one("SELECT count(*) FROM pg_class WHERE oid IN ('execution_recovery_cases'::regclass,'execution_account_mappings'::regclass,'execution_recovery_history'::regclass,'execution_recovery_targets'::regclass,'execution_recovery_acks'::regclass,'execution_fee_proposals'::regclass,'execution_fee_approvals'::regclass) AND relrowsecurity AND relforcerowsecurity",&[]).await?.get(0);
    if n != 7 {
        return Err(Error::UnsafeRole);
    }
    Ok(tx)
}
pub async fn load_case(client: &mut Client, tenant: &str, id: &str) -> Result<Case, Error> {
    if !text(id, 256) {
        return Err(Error::InvalidInput);
    }
    let tx = scope(client, tenant).await?;
    let limit = HISTORY_BYTES as i64;
    // A corrupt restored TEXT/BYTEA must be bounded before transfer, not after
    // decoding. Metadata and observation bytes have independent budgets.
    let row=tx.query_typed_opt("SELECT CASE WHEN octet_length(case_id)::bigint+octet_length(order_id)+octet_length(COALESCE(mapping_version,''))+octet_length(source_cursor)+octet_length(payload_digest)+octet_length(status)+octet_length(reason)<=8192 THEN ARRAY[case_id,order_id,COALESCE(mapping_version,''),source_cursor,payload_digest,status,reason] END,CASE WHEN octet_length(evidence)::bigint<=$2 THEN evidence END,checkpoint,version FROM execution_recovery_cases WHERE case_id=$1",&[(&id, Type::TEXT), (&limit, Type::INT8)]).await?.ok_or(Error::NotFound)?;
    let f: Vec<String> = row
        .get::<_, Option<Vec<String>>>(0)
        .ok_or(Error::Capacity)?;
    let evidence: Vec<u8> = row.get::<_, Option<Vec<u8>>>(1).ok_or(Error::Capacity)?;
    let c = Case {
        id: f[0].clone(),
        order_id: f[1].clone(),
        mapping_version: f[2].clone(),
        source_cursor: f[3].clone(),
        digest: f[4].clone(),
        status: f[5].clone(),
        reason: f[6].clone(),
        evidence,
        checkpoint: row.get(2),
        version: row.get(3),
    };
    if c.checkpoint < 0 || c.checkpoint as usize > MAX_FILLS {
        return Err(Error::InvalidInput);
    }
    commit(tx).await?;
    Ok(c)
}
pub async fn observe(
    client: &mut Client,
    tenant: &str,
    id: &str,
    order: &str,
    cursor: &str,
    raw: &[u8],
) -> Result<Case, Error> {
    if !text(id, 256)
        || !text(order, 256)
        || !text(cursor, 1024)
        || raw.is_empty()
        || raw.len() > HISTORY_BYTES
    {
        return Err(Error::InvalidInput);
    }
    let hash = digest(raw);
    let tx = scope(client, tenant).await?;
    tx.execute("INSERT INTO execution_recovery_cases (case_id,order_id,source_cursor,payload_digest,evidence,status) VALUES ($1,$2,$3,$4,$5,'observed') ON CONFLICT (tenant_id,case_id) DO NOTHING",&[&id,&order,&cursor,&hash,&raw]).await?;
    // Go persists the observation before reading/refusing conflicting evidence.
    // Do not claim rollback of this acknowledged observation on later failure.
    commit(tx).await?;
    let case = load_case(client, tenant, id).await?;
    if case.order_id != order || case.source_cursor != cursor || case.digest != hash {
        return Err(Error::IdentityConflict);
    }
    Ok(case)
}
/// Caller constructs Mapping only from its configured binding and registered
/// adapter identity. No caller-controlled `verified` boolean enters storage.
pub async fn record_mapping(client: &mut Client, mapping: &Mapping<'_>) -> Result<String, Error> {
    let version = mapping.version()?;
    let tx = scope(client, mapping.tenant).await?;
    tx.execute("INSERT INTO execution_account_mappings (tenant_id,version,venue,venue_account_id,exchange_account_id,portfolio_id) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (tenant_id,version) DO NOTHING",&[&mapping.tenant,&version,&mapping.venue,&mapping.account,&mapping.exchange_account,&mapping.portfolio]).await?;
    let matched:bool=tx.query_one("SELECT EXISTS(SELECT 1 FROM execution_account_mappings WHERE tenant_id=$1 AND version=$2 AND venue=$3 AND venue_account_id=$4 AND exchange_account_id=$5 AND portfolio_id=$6)",&[&mapping.tenant,&version,&mapping.venue,&mapping.account,&mapping.exchange_account,&mapping.portfolio]).await?.get(0);
    if !matched {
        return Err(Error::IdentityConflict);
    }
    commit(tx).await?;
    Ok(version)
}
/// Revalidate the immutable mapping against the currently loaded order before
/// preparing a recovery. A frozen history alone is not proof that the mapping
/// survived restoration or still attributes this order.
pub async fn mapping_matches(
    client: &mut Client,
    tenant: &str,
    version: &str,
    state: &OrderState,
) -> Result<bool, Error> {
    if !text(version, 256) {
        return Err(Error::InvalidInput);
    }
    let tx = scope(client, tenant).await?;
    let matched = tx.query_one("SELECT EXISTS(SELECT 1 FROM execution_account_mappings WHERE version=$1 AND portfolio_id=$2 AND venue=$3 AND venue_account_id=$4)", &[&version, &state.portfolio_id, &state.venue, &state.venue_account_id]).await?.get(0);
    commit(tx).await?;
    Ok(matched)
}
fn status_record(c: &Case, st: &OrderState, r: &Record, tenant: &str) -> Result<(), Error> {
    validate_batch(&st.encode_to_vec(), std::slice::from_ref(r), 1, MAX_BYTES)?;
    let v =
        ExecutionRecoveryRecorded::decode(r.payload.as_slice()).map_err(|_| Error::InvalidInput)?;
    let at = recovery::time(v.recorded_at.as_ref().ok_or(Error::InvalidInput)?)?;
    recovery::fact(
        r,
        tenant,
        &c.order_id,
        "order.order.recovery_recorded",
        "order.v1.ExecutionRecoveryRecorded:1",
        at,
    )?;
    if v.case_id != c.id
        || v.order_id != c.order_id
        || v.portfolio_id != st.portfolio_id
        || v.venue != st.venue
        || v.venue_account_id != st.venue_account_id
        || v.mapping_version != c.mapping_version
        || v.source_cursor != c.source_cursor
        || v.payload_digest != c.digest
        || v.status != c.status
        || v.reason != c.reason
        || v.checkpoint as i32 != c.checkpoint
    {
        return Err(Error::IdentityConflict);
    }
    Ok(())
}
pub async fn freeze(
    client: &mut Client,
    tenant: &str,
    c: &Case,
    st: &OrderState,
    history: &Canonical<ExecutionRecoveryHistory>,
    record: &Record,
) -> Result<(), Error> {
    let h = history.value();
    if h.order_id != c.order_id
        || st.order_id != c.order_id
        || !text(&h.mapping_version, 256)
        || h.fills.len() > MAX_FILLS
        || history.wire().len() > HISTORY_BYTES
    {
        return Err(Error::InvalidInput);
    }
    let at = recovery::time(h.observed_at.as_ref().ok_or(Error::InvalidInput)?)?;
    if at != record.event_time {
        return Err(Error::IdentityConflict);
    }
    let mut next = c.clone();
    next.mapping_version = h.mapping_version.clone();
    next.status = "investigating".into();
    next.reason.clear();
    status_record(&next, st, record, tenant)?;
    let tx = scope(client, tenant).await?;
    let matched:bool=tx.query_one("SELECT EXISTS(SELECT 1 FROM execution_account_mappings WHERE version=$1 AND portfolio_id=$2 AND venue=$3 AND venue_account_id=$4)",&[&h.mapping_version,&st.portfolio_id,&st.venue,&st.venue_account_id]).await?.get(0);
    if !matched {
        return Err(Error::IdentityConflict);
    }
    let n=tx.execute("UPDATE execution_recovery_cases SET mapping_version=$1,status='investigating',reason='',version=version+1,updated_at=$2 WHERE case_id=$3 AND order_id=$4 AND version=$5 AND status IN ('observed','blocked')",&[&h.mapping_version,&at,&c.id,&c.order_id,&c.version]).await?;
    if n != 1 {
        return Err(Error::Conflict);
    }
    let hash = digest(history.wire());
    tx.execute("INSERT INTO execution_recovery_history (case_id,mapping_version,payload_digest,history) VALUES ($1,$2,$3,$4)",&[&c.id,&h.mapping_version,&hash,&history.wire()]).await?;
    enqueue(&tx, std::slice::from_ref(record)).await?;
    commit(tx).await
}
pub async fn history(
    client: &mut Client,
    tenant: &str,
    id: &str,
) -> Result<(Canonical<ExecutionRecoveryHistory>, String), Error> {
    let tx = scope(client, tenant).await?;
    let limit = HISTORY_BYTES as i64;
    let row=tx.query_opt("SELECT CASE WHEN octet_length(h.history)::bigint<=$2 THEN h.history END,CASE WHEN octet_length(h.payload_digest)::bigint+octet_length(c.order_id)+octet_length(c.mapping_version)<=1024 THEN ARRAY[h.payload_digest,c.order_id,c.mapping_version] END FROM execution_recovery_history h JOIN execution_recovery_cases c USING (tenant_id,case_id) WHERE h.case_id=$1",&[&id,&limit]).await?.ok_or(Error::NotFound)?;
    let raw: Vec<u8> = row.get::<_, Option<Vec<u8>>>(0).ok_or(Error::Capacity)?;
    let meta: Vec<String> = row
        .get::<_, Option<Vec<String>>>(1)
        .ok_or(Error::Capacity)?;
    if digest(&raw) != meta[0] {
        return Err(Error::IdentityConflict);
    }
    let h =
        Canonical::<ExecutionRecoveryHistory>::decode_with_budget(&raw, Budget::RecoveryHistory)?;
    if h.value().order_id != meta[1] || h.value().mapping_version != meta[2] {
        return Err(Error::IdentityConflict);
    }
    commit(tx).await?;
    Ok((h, meta[0].clone()))
}
pub async fn block(
    client: &mut Client,
    tenant: &str,
    c: &Case,
    st: &OrderState,
    reason: &str,
    record: &Record,
) -> Result<(), Error> {
    if !text(reason, 2048) || st.order_id != c.order_id {
        return Err(Error::InvalidInput);
    }
    let mut next = c.clone();
    next.status = "blocked".into();
    next.reason = reason.into();
    status_record(&next, st, record, tenant)?;
    let tx = scope(client, tenant).await?;
    let n=tx.execute("UPDATE execution_recovery_cases SET status='blocked',reason=$1,version=version+1,updated_at=$2 WHERE case_id=$3 AND order_id=$4 AND version=$5 AND status<>'corrected'",&[&reason,&record.event_time,&c.id,&c.order_id,&c.version]).await?;
    if n != 1 {
        return Err(Error::Conflict);
    }
    enqueue(&tx, std::slice::from_ref(record)).await?;
    commit(tx).await
}
pub async fn target_matches(
    client: &mut Client,
    tenant: &str,
    case: &str,
    key: &str,
    payload: &str,
    execution: &str,
) -> Result<bool, Error> {
    let tx = scope(client, tenant).await?;
    let matched=tx.query_one("SELECT EXISTS(SELECT 1 FROM execution_recovery_targets WHERE case_id=$1 AND execution_key=$2 AND payload_digest=$3 AND execution_digest=$4)",&[&case,&key,&payload,&execution]).await?.get(0);
    commit(tx).await?;
    Ok(matched)
}

#[cfg(test)]
mod reader_tests {
    use super::*;
    use kanz_order_core::proto::order::v1::Fill;
    #[test]
    fn malformed_identity_cannot_accumulate_unbounded_fallback_keys() {
        let fill = Fill {
            fill_id: "x".repeat(MAX_BYTES),
            ..Default::default()
        };
        assert!(matches!(
            posted_key(&fill),
            Err(Error::PostedExecutionUnknown)
        ));
        let fill = Fill {
            fill_id: "legacy".into(),
            ..Default::default()
        };
        assert!(matches!(
            posted_key(&fill),
            Err(Error::PostedExecutionUnknown)
        ));
        let fill = Fill {
            fill_id: "fill".into(),
            venue: "venue".into(),
            venue_account_id: "account".into(),
            instrument_id: "instrument".into(),
            venue_execution_id: "execution".into(),
            ..Default::default()
        };
        assert_eq!(posted_key(&fill).unwrap(), identity::execution_key(&fill));
    }
}

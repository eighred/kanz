//! Durable tail of the existing human fee-correction commands. Authentication,
//! entitlement and two-person approval remain the typed command owner's job.
//! Each write shares its CAS and FACT enqueue in one canonical RLS transaction.
use super::{
    cas, commit, enqueue, fee_subject, recovery_repository as repository, validate_batch, Error,
    Record, MAX_BYTES,
};
use kanz_order_core::{
    proto::order::v1::{ExecutionFeeCorrectionApproved, ExecutionFeeCorrectionProposed},
    Canonical,
};
use tokio_postgres::Client;

pub const PROPOSAL_BYTES: usize = 512 * 1024;
pub struct Proposal {
    pub raw: Vec<u8>,
    pub digest: String,
}
pub async fn proposal(
    client: &mut Client,
    tenant: &str,
    case: &str,
) -> Result<Option<Proposal>, Error> {
    if !repository::text(case, 256) {
        return Err(Error::InvalidInput);
    }
    let tx = repository::scope(client, tenant).await?;
    let limit = PROPOSAL_BYTES as i64;
    let row = tx.query_opt("SELECT CASE WHEN octet_length(proposal)::bigint<=$2 THEN proposal END,CASE WHEN octet_length(digest)<=64 THEN digest END FROM execution_fee_proposals WHERE case_id=$1", &[&case, &limit]).await?;
    let result = row
        .map(|r| {
            Ok::<Proposal, Error>(Proposal {
                raw: r.get::<_, Option<Vec<u8>>>(0).ok_or(Error::Capacity)?,
                digest: r.get::<_, Option<String>>(1).ok_or(Error::Capacity)?,
            })
        })
        .transpose()?;
    commit(tx).await?;
    Ok(result)
}
pub async fn approval(
    client: &mut Client,
    tenant: &str,
    case: &str,
) -> Result<Option<Vec<u8>>, Error> {
    if !repository::text(case, 256) {
        return Err(Error::InvalidInput);
    }
    let tx = repository::scope(client, tenant).await?;
    let result = approval_in(&tx, case).await?;
    commit(tx).await?;
    Ok(result)
}
async fn approval_in(
    tx: &tokio_postgres::Transaction<'_>,
    case: &str,
) -> Result<Option<Vec<u8>>, Error> {
    let limit = MAX_BYTES as i64;
    let row = tx.query_opt("SELECT CASE WHEN octet_length(approval)::bigint<=$2 THEN approval END FROM execution_fee_approvals WHERE case_id=$1", &[&case, &limit]).await?;
    row.map(|r| r.get::<_, Option<Vec<u8>>>(0).ok_or(Error::Capacity))
        .transpose()
}
fn record(
    tenant: &str,
    order: &str,
    fact: &Record,
    subject: &str,
    schema: &str,
    payload: &[u8],
) -> Result<(), Error> {
    if fact.tenant != tenant
        || fact.partition_key != order
        || fact.subject != subject
        || fact.event_type != subject
        || fact.event_class != 1
        || fact.schema_version != 1
        || fact.domain != "order"
        || fact.payload_schema_ref != schema
        || fact.payload != payload
    {
        return Err(Error::InvalidInput);
    }
    Ok(())
}
pub async fn save_proposal(
    client: &mut Client,
    tenant: &str,
    state: &[u8],
    version: i64,
    proposal: &Canonical<ExecutionFeeCorrectionProposed>,
    fact: &Record,
) -> Result<(), Error> {
    let p = proposal.value();
    if proposal.wire().len() > PROPOSAL_BYTES
        || !repository::text(&p.case_id, 256)
        || !repository::text(&p.order_id, 256)
        || p.digest.len() != 64
    {
        return Err(Error::InvalidInput);
    }
    let st = validate_batch(state, std::slice::from_ref(fact), 1, 2 * MAX_BYTES)?;
    if st.order_id != p.order_id || st.portfolio_id != p.portfolio_id {
        return Err(Error::InvalidInput);
    }
    record(
        tenant,
        &p.order_id,
        fact,
        "order.order.fee_correction_proposed",
        "order.v1.ExecutionFeeCorrectionProposed:1",
        proposal.wire(),
    )?;
    let tx = repository::scope(client, tenant).await?;
    cas(&tx, &st, state, version).await?;
    tx.execute(
        "INSERT INTO execution_fee_proposals(case_id,order_id,digest,proposal) VALUES($1,$2,$3,$4)",
        &[&p.case_id, &p.order_id, &p.digest, &proposal.wire()],
    )
    .await?;
    enqueue(&tx, std::slice::from_ref(fact)).await?;
    commit(tx).await
}
pub async fn save_approval(
    client: &mut Client,
    tenant: &str,
    case: &repository::Case,
    state: &[u8],
    approved: &Canonical<ExecutionFeeCorrectionApproved>,
    fact: &Record,
) -> Result<(), Error> {
    let a = approved.value();
    let st = validate_batch(state, std::slice::from_ref(fact), 1, 2 * MAX_BYTES)?;
    if !repository::text(&case.id, 256)
        || a.case_id != case.id
        || a.order_id != case.order_id
        || st.order_id != case.order_id
    {
        return Err(Error::InvalidInput);
    }
    record(
        tenant,
        &a.order_id,
        fact,
        "order.order.fee_correction_approved",
        "order.v1.ExecutionFeeCorrectionApproved:1",
        approved.wire(),
    )?;
    let tx = repository::scope(client, tenant).await?;
    // Match Go's second check inside the transaction. A concurrent signature is
    // idempotent only for the same digest and normalized authenticated subject.
    if let Some(raw) = approval_in(&tx, &case.id).await? {
        let old = Canonical::<ExecutionFeeCorrectionApproved>::decode(&raw)?;
        if old.value().digest != a.digest
            || fee_subject::normalize(&old.value().approver) != fee_subject::normalize(&a.approver)
        {
            return Err(Error::IdentityConflict);
        }
        return commit(tx).await;
    }
    if case.checkpoint != 0 || !matches!(case.status.as_str(), "blocked" | "investigating") {
        return Err(Error::Conflict);
    }
    let count = tx.execute("UPDATE execution_recovery_cases SET status='investigating',reason='',version=version+1,updated_at=$1 WHERE case_id=$2 AND version=$3 AND checkpoint=0 AND status IN ('blocked','investigating')", &[&fact.event_time, &case.id, &case.version]).await?;
    if count != 1 {
        return Err(Error::Conflict);
    }
    tx.execute(
        "INSERT INTO execution_fee_approvals(case_id,approval) VALUES($1,$2)",
        &[&case.id, &approved.wire()],
    )
    .await?;
    enqueue(&tx, std::slice::from_ref(fact)).await?;
    commit(tx).await
}

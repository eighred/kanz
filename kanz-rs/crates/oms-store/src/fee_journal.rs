//! Fee append primitive for an existing caller-owned transaction. It neither
//! begins nor commits transactions and does not authenticate approval evidence.
use super::{cas, enqueue, scope_existing, validate_write, Error, Record, MAX_BYTES};
use super::{fee_revision, fee_wire::FillWire, identity};
use num_rational::BigRational;
use num_traits::Zero;
use tokio_postgres::Transaction;

pub struct OrderCas<'tx, 'connection> {
    tx: &'tx mut Transaction<'connection>,
    tenant: String,
    order_id: String,
    state: Vec<u8>,
}
pub struct Revision {
    pub delta: BigRational,
    pub fresh: bool,
}
/// The caller owns the transaction, original execution provenance, admission,
/// recovery checkpoint, announcements and final Commit/Rollback. CAS retains
/// the existing Go upsert semantics; it is not proof that a trade was observed.
/// Any error after CAS requires caller rollback of the entire transaction.
pub async fn cas_order<'tx, 'connection>(
    tx: &'tx mut Transaction<'connection>,
    tenant: &str,
    state: &[u8],
    expected_version: i64,
) -> Result<OrderCas<'tx, 'connection>, Error> {
    let st = validate_write(state, &[])?;
    scope_existing(tx, tenant).await?;
    cas(tx, &st, state, expected_version).await?;
    Ok(OrderCas {
        tx,
        tenant: tenant.to_owned(),
        order_id: st.order_id,
        state: state.to_vec(),
    })
}
impl OrderCas<'_, '_> {
    async fn scoped(&self) -> Result<(), Error> {
        let current: Option<String> = self
            .tx
            .query_one("SELECT current_setting('app.tenant_id',true)", &[])
            .await?
            .try_get(0)?;
        if current.as_deref() != Some(self.tenant.as_str()) {
            return Err(Error::InvalidInput);
        }
        Ok(())
    }
    pub async fn head(&self, original: &FillWire) -> Result<FillWire, Error> {
        self.scoped().await?;
        if original.value().order_id != self.order_id {
            return Err(Error::FeeRevision);
        }
        head(self.tx, original).await
    }
    pub async fn record_fee_revision(
        &mut self,
        original: &FillWire,
        revised: &FillWire,
    ) -> Result<Revision, Error> {
        self.scoped().await?;
        if original.value().order_id != self.order_id
            || revised.value().order_id != self.order_id
            || !identity::same_except_fee(original.value(), revised.value())
        {
            return Err(Error::FeeRevision);
        }
        record_for(self.tx, Book::Oms, original, revised).await
    }
    pub async fn enqueue(&self, records: &[Record]) -> Result<(), Error> {
        self.scoped().await?;
        validate_write(&self.state, records)?;
        enqueue(self.tx, records).await
    }
}
/// Shared latest-head semantics for duplicate fills and fee append. Missing
/// head means the original execution, not a fabricated zero-fee fill.
pub(crate) async fn head(tx: &Transaction<'_>, original: &FillWire) -> Result<FillWire, Error> {
    head_for(tx, Book::Oms, original).await
}
pub(crate) async fn head_for(
    tx: &Transaction<'_>,
    book: Book,
    original: &FillWire,
) -> Result<FillWire, Error> {
    let key = identity::execution_key(original.value());
    let row=tx.query_opt("SELECT CASE WHEN octet_length(revised_fill)::bigint<=$2 THEN revised_fill END FROM execution_fee_revisions WHERE book=$3 AND execution_key=$1 ORDER BY revision_sequence DESC LIMIT 1",&[&key,&(MAX_BYTES as i64),&book.name()]).await?;
    let Some(row) = row else {
        return Ok(original.clone());
    };
    let raw: Vec<u8> = row
        .try_get::<_, Option<Vec<u8>>>(0)?
        .ok_or(Error::Capacity)?;
    let revised = FillWire::decode(&raw)?;
    if !identity::same_except_fee(original.value(), revised.value()) {
        return Err(Error::FeeRevision);
    }
    Ok(revised)
}

#[derive(Clone, Copy)]
pub(crate) enum Book {
    Oms,
    Position,
}
impl Book {
    fn name(self) -> &'static str {
        match self {
            Self::Oms => "oms",
            Self::Position => "position",
        }
    }
}
pub(crate) async fn record_for(
    tx: &Transaction<'_>,
    book: Book,
    original: &FillWire,
    revised: &FillWire,
) -> Result<Revision, Error> {
    if !identity::same_except_fee(original.value(), revised.value()) {
        return Err(Error::FeeRevision);
    }
    fee_revision::terms(revised)?;
    let f = revised.value();
    let key = identity::execution_key(f);
    let recovery = f.recovery.as_ref().ok_or(Error::FeeRevision)?;
    let prior=tx.query_opt("SELECT CASE WHEN octet_length(revised_fill)::bigint<=$3 THEN revised_fill END FROM execution_fee_revisions WHERE book=$4 AND execution_key=$1 AND case_id=$2",&[&key,&recovery.case_id,&(MAX_BYTES as i64), &book.name()]).await?;
    if let Some(row) = prior {
        let raw: Vec<u8> = row
            .try_get::<_, Option<Vec<u8>>>(0)?
            .ok_or(Error::Capacity)?;
        let recorded = FillWire::decode(&raw)?;
        if !recorded.equivalent(revised) {
            return Err(Error::FeeRevision);
        }
        return Ok(Revision {
            delta: BigRational::zero(),
            fresh: false,
        });
    }
    let previous = head_for(tx, book, original).await?;
    let delta = fee_revision::approved_delta(&previous, revised)?;
    let before = previous.journal_bytes()?;
    let after = revised.journal_bytes()?;
    let approval = &recovery
        .fee_approval
        .as_ref()
        .ok_or(Error::FeeRevision)?
        .digest;
    tx.execute("INSERT INTO execution_fee_revisions (book,execution_key,case_id,approval_digest,previous_fill,revised_fill,order_id) VALUES ($7,$1,$2,$3,$4,$5,$6)",&[&key,&recovery.case_id,approval,&before,&after,&f.order_id,&book.name()]).await?;
    Ok(Revision { delta, fresh: true })
}

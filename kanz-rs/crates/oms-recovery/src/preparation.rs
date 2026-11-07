use crate::{approval::ApprovedChanges, history, Error};
use kanz_oms_store::{identity, recovery_repository};
use kanz_order_core::proto::order::v1::ExecutionRecoveryHistory;
use kanz_order_core::{
    proto::order::v1::{Fill, OrderState},
    rational, Canonical,
};
use num_rational::BigRational;
use num_traits::{Signed, Zero};
use std::collections::{BTreeMap, BTreeSet};
use tokio_postgres::Client;

pub(crate) struct Projection {
    pub state: Canonical<OrderState>,
    pub fills: Vec<kanz_oms_store::recovery_wire::Execution>,
    pub missing: BTreeSet<String>,
}
pub(crate) enum ProjectionError {
    Block,
    Error(Error),
}
impl From<Error> for ProjectionError {
    fn from(e: Error) -> Self {
        Self::Error(e)
    }
}
impl From<kanz_proto_wire::Error> for ProjectionError {
    fn from(e: kanz_proto_wire::Error) -> Self {
        Self::Error(e.into())
    }
}
fn aggregate_error(e: kanz_order_core::aggregate::Error) -> Error {
    match e {
        kanz_order_core::aggregate::Error::Capacity => Error::Capacity,
        _ => Error::PostedHistoryUnknown,
    }
}
pub(crate) fn project(
    state: &Canonical<OrderState>,
    case: &recovery_repository::Case,
    history: &Canonical<ExecutionRecoveryHistory>,
    digest: &str,
    approvals: &ApprovedChanges,
    posted: &BTreeSet<String>,
    now: prost_types::Timestamp,
) -> Result<Projection, ProjectionError> {
    let originals = history::from_frozen(state, history)?;
    let observed = history
        .message_wire("observed_at")?
        .ok_or(Error::EvidenceConflict)?;
    let mut next = state.clone();
    let mut fills = Vec::with_capacity(originals.len());
    let mut missing = BTreeSet::new();
    let mut cost = BigRational::zero();
    let context = kanz_oms_store::recovery_wire::Context::new(
        case.id.clone(),
        case.mapping_version.clone(),
        case.source_cursor.clone(),
        digest.into(),
        observed,
    )
    .map_err(Error::from)?;
    for original in originals {
        let f = original.value();
        let key = identity::execution_key(f);
        cost += rational(f.quantity.as_ref()).ok_or(Error::HistoryIncomplete)?
            * rational(f.price.as_ref()).ok_or(Error::HistoryIncomplete)?;
        let recipe = kanz_oms_store::recovery_wire::Execution::new(
            kanz_oms_store::fee_wire::FillWire::decode(original.wire()).map_err(Error::from)?,
            std::sync::Arc::clone(&context),
            approvals.terms(&key)?,
        )
        .map_err(Error::from)?;
        let materialized = recipe.materialize().map_err(Error::from)?;
        let recovered = Canonical::<Fill>::decode(materialized.original())?;
        if !posted.contains(&key) {
            next = kanz_order_core::aggregate::apply_recovered_fill(&next, &recovered, now)
                .map_err(|e| {
                    use kanz_order_core::aggregate::RecoveryFoldError as E;
                    match e {
                        E::Fold(_) => ProjectionError::Block,
                        E::Evidence => ProjectionError::Error(Error::EvidenceConflict),
                        E::PostedUnknown => ProjectionError::Error(Error::PostedHistoryUnknown),
                        E::Representation(e) => ProjectionError::Error(aggregate_error(e)),
                    }
                })?;
            missing.insert(key);
        }
        fills.push(recipe);
    }
    let total = history
        .value()
        .executed_quantity
        .as_ref()
        .ok_or(Error::HistoryIncomplete)?;
    if rational(next.value().filled_quantity.as_ref()) != rational(Some(total)) {
        return Err(Error::PostedHistoryUnknown.into());
    }
    next = kanz_order_core::aggregate::recovery_average(&next, cost, total)
        .map_err(aggregate_error)?;
    Ok(Projection {
        state: next,
        fills,
        missing,
    })
}

/// Validate every existing execution and fee baseline in a consistent SQL
/// snapshot, keeping only identities. Raw journal records are transferred one
/// per bounded keyset query and released before the next query.
pub(crate) enum PostedError {
    Snapshot(Error),
    Evidence(Error),
}
impl From<kanz_oms_store::Error> for PostedError {
    fn from(e: kanz_oms_store::Error) -> Self {
        Self::Snapshot(e.into())
    }
}
pub(crate) async fn posted(
    client: &mut Client,
    tenant: &str,
    state: &Canonical<OrderState>,
    observed: &[Canonical<Fill>],
    approvals: Option<&ApprovedChanges>,
) -> Result<BTreeSet<String>, PostedError> {
    let st = state.value();
    let observed: BTreeMap<_, _> = observed
        .iter()
        .map(|f| (identity::execution_key(f.value()), f))
        .collect();
    let mut baselines: BTreeMap<&str, Vec<&Canonical<Fill>>> = BTreeMap::new();
    if let Some(approvals) = approvals {
        for (key, fill) in &approvals.baselines {
            baselines.entry(key).or_default().push(fill);
        }
    }
    let mut cursor = recovery_repository::posted_reader(client, tenant, &st.order_id).await?;
    let mut seen = BTreeSet::new();
    let mut quantity = BigRational::zero();
    let mut evidence_error = None;
    while let Some(row) = cursor.next().await? {
        let f = row.original.value();
        let q = rational(f.quantity.as_ref())
            .ok_or(PostedError::Snapshot(Error::PostedHistoryUnknown))?;
        if history::domain(f).is_err()
            || f.venue_account_id != st.venue_account_id
            || f.venue != st.venue
            || f.instrument_id != st.instrument_id
            || f.order_id != st.order_id
            || f.side != st.side
            || f.venue_execution_id.is_empty()
            || !q.is_positive()
        {
            return Err(PostedError::Snapshot(Error::PostedHistoryUnknown));
        }
        let key = identity::execution_key(f);
        if let Some(approvals) = approvals {
            match observed.get(&key) {
                Some(next)
                    if identity::same_execution(row.current.value(), next.value())
                        || (approvals.by_execution.contains_key(&key)
                            && identity::same_except_fee(row.current.value(), next.value())) => {}
                _ => {
                    evidence_error.get_or_insert(Error::EvidenceConflict);
                }
            }
        }
        if let Some(previous) = baselines.get(key.as_str()) {
            for previous in previous {
                if !identity::same_execution(row.current.value(), previous.value()) {
                    evidence_error.get_or_insert(Error::FeeRevision);
                }
            }
        }
        quantity += q;
        seen.insert(key);
    }
    cursor.finish().await?;
    if quantity
        != rational(st.filled_quantity.as_ref())
            .ok_or(PostedError::Snapshot(Error::PostedHistoryUnknown))?
    {
        return Err(PostedError::Snapshot(Error::PostedHistoryUnknown));
    }
    if baselines.keys().any(|key| !seen.contains(*key)) {
        return Err(PostedError::Evidence(Error::FeeRevision));
    }
    if let Some(error) = evidence_error {
        return Err(PostedError::Evidence(error));
    }
    Ok(seen)
}

//! Durable observation phases; a blocked handler acknowledgement is not recovery success.
use crate::{approval, handlers, history, preparation, Error};
use kanz_oms_store::{self as store, recovery, recovery_repository as repository};
use kanz_order_core::{
    proto::{
        envelope::v1::Envelope,
        order::v1::{Fill, OrderState, StateHealed},
    },
    Canonical,
};
use kanz_proto_wire::Budget;
use std::time::Duration;
use tokio_postgres::Client;

pub(crate) struct Binding {
    pub tenant: String,
    pub portfolio: String,
    pub mic: String,
    pub account: String,
}
async fn blocked(
    client: &mut Client,
    tenant: &str,
    env: &Envelope,
    c: &repository::Case,
    st: &OrderState,
    reason: &str,
    now: prost_types::Timestamp,
) -> Result<String, Error> {
    let mut next = c.clone();
    next.status = "blocked".into();
    next.reason = reason.into();
    let record = handlers::status(env, &next, st, now)?;
    repository::block(client, tenant, c, st, reason, &record).await?;
    Ok(c.order_id.clone())
}
async fn prepared(
    client: &mut Client,
    tenant: &str,
    env: &Envelope,
    c: &repository::Case,
    now: prost_types::Timestamp,
) -> Result<(), preparation::ProjectionError> {
    let loaded = store::load(client, tenant, &c.order_id)
        .await
        .map_err(Error::from)?;
    let st = Canonical::<OrderState>::decode(&loaded.state)?;
    let (history, digest) = repository::history(client, tenant, &c.id)
        .await
        .map_err(|error| match error {
            store::Error::IdentityConflict => Error::RecoveryEvidenceConflict,
            other => other.into(),
        })?;
    let fills = history::from_frozen(&st, &history)?;
    if !repository::mapping_matches(client, tenant, &c.mapping_version, st.value())
        .await
        .map_err(Error::from)?
    {
        return Err(Error::RecoveryEvidenceConflict.into());
    }
    let approvals = match repository::fee_evidence(client, tenant, &c.id).await {
        Ok(evidence) => approval::approved(tenant, c, &digest, evidence),
        Err(error) => Err(error.into()),
    };
    // Posted-state validity precedes approval refusal in the Go owner. Retain
    // a parsed approval failure until that snapshot has been checked.
    let posted =
        match preparation::posted(client, tenant, &st, &fills, approvals.as_ref().ok()).await {
            Ok(posted) => posted,
            Err(preparation::PostedError::Snapshot(error)) => {
                // Match Go's optimistic snapshot retry: a concurrently advanced
                // order must not be durably blocked by evidence read against its
                // previous version. Failed reload retains the original error.
                if store::load(client, tenant, &c.order_id)
                    .await
                    .is_ok_and(|latest| latest.version != loaded.version)
                {
                    return Err(Error::Conflict.into());
                }
                return Err(error.into());
            }
            Err(preparation::PostedError::Evidence(error)) => return Err(error.into()),
        };
    let approvals = approvals?;
    let projected = preparation::project(&st, c, &history, &digest, &approvals, &posted, now)?;
    let mut next = c.clone();
    next.checkpoint = projected.fills.len() as i32;
    next.status = if projected.fills.is_empty() {
        "corrected"
    } else {
        "investigating"
    }
    .into();
    next.reason = if projected.fills.is_empty() {
        ""
    } else {
        "OMS committed execution evidence; awaiting position and ledger acknowledgements"
    }
    .into();
    let status = handlers::status(env, &next, projected.state.value(), now)?;
    let batch = recovery::StreamedRecovery {
        state: projected.state.wire().to_vec(),
        order_version: loaded.version,
        case_version: c.version,
        history_digest: digest,
        missing: projected.missing.into_iter().collect(),
        executions: projected.fills,
        status,
    };
    recovery::commit_streamed_recovery(client, tenant, &batch)
        .await
        .map_err(Error::from)?;
    Ok(())
}
/// Network awaits occur only between completed SQL transactions. SQL CAS remains
/// the cross-process fence; the owning coordinator serializes its own commands.
pub(crate) async fn observe(
    client: &mut Client,
    venue: &kanz_oms_venue::Client,
    binding: &Binding,
    env: &Envelope,
    payload: &[u8],
    now: prost_types::Timestamp,
    deadline: Duration,
) -> Result<String, Error> {
    let tenant = binding.tenant.as_str();
    if env.event_type != "order.order.healed"
        || payload.is_empty()
        || payload.len() > repository::HISTORY_BYTES
    {
        return Err(Error::InvalidInput);
    }
    let observation =
        Canonical::<StateHealed>::decode_with_budget(payload, Budget::RecoveryHistory)?;
    let o = observation.value();
    if !repository::text(&o.order_id, 256) || !repository::text(&o.venue, 256) {
        return Err(Error::InvalidInput);
    }
    let mut c = repository::observe(
        client,
        tenant,
        &env.event_id,
        &o.order_id,
        &env.event_id,
        payload,
    )
    .await?;
    for _ in 0..4 {
        if c.status == "corrected" || c.status == "blocked" || c.checkpoint > 0 {
            return Ok(c.order_id);
        }
        let loaded = match store::load(client, tenant, &c.order_id).await {
            Ok(v) => v,
            Err(store::Error::NotFound) => {
                return blocked(
                    client,
                    tenant,
                    env,
                    &c,
                    &OrderState {
                        order_id: c.order_id.clone(),
                        venue: o.venue.clone(),
                        ..Default::default()
                    },
                    "observed order is not attributable to this tenant's OMS",
                    now,
                )
                .await
            }
            Err(e) => return Err(e.into()),
        };
        let state = Canonical::<OrderState>::decode(&loaded.state)?;
        let st = state.value();
        if o.venue != st.venue
            || o.state.as_ref().is_some_and(|s| {
                (!s.order_id.is_empty() && s.order_id != st.order_id)
                    || (!s.venue_account_id.is_empty() && s.venue_account_id != st.venue_account_id)
            })
        {
            return blocked(
                client,
                tenant,
                env,
                &c,
                st,
                "observation conflicts with the persisted order attribution",
                now,
            )
            .await;
        }
        if c.status == "observed" {
            if binding.portfolio != st.portfolio_id
                || binding.account != st.venue_account_id
                || binding.mic != st.venue
            {
                return blocked(
                    client,
                    tenant,
                    env,
                    &c,
                    st,
                    "no explicit portfolio-to-venue-account binding matches the order",
                    now,
                )
                .await;
            }
            let identity = venue.describe(deadline).await.map_err(|_| Error::Venue)?;
            let id = identity.value();
            if id.mic != st.venue
                || id.account != binding.account
                || !id.account_verified
                || id.exchange_account_id.is_empty()
            {
                return blocked(
                    client,
                    tenant,
                    env,
                    &c,
                    st,
                    "venue account proof does not match the persisted order attribution",
                    now,
                )
                .await;
            }
            let mapping = repository::record_mapping(
                client,
                &repository::Mapping {
                    tenant,
                    portfolio: &st.portfolio_id,
                    venue: &st.venue,
                    account: &binding.account,
                    exchange_account: &id.exchange_account_id,
                },
            )
            .await?;
            let venue_state = kanz_oms_venue::Canonical::from(state.clone());
            let view = venue
                .query_recovery_history(&venue_state, deadline)
                .await
                .map_err(|e| match e.code() as i32 {
                    8 | 11 => Error::Capacity,
                    4 => Error::Deadline,
                    _ => Error::Venue,
                })?;
            let total = view
                .response
                .as_ref()
                .and_then(|r| r.value().executed_quantity.as_ref());
            let fills = view
                .fills
                .iter()
                .map(|f| {
                    Canonical::<Fill>::decode_with_budget(f.wire(), Budget::RecoveryHistory)
                        .map_err(Error::from)
                })
                .collect::<Result<Vec<_>, _>>()?;
            let fills=match history::complete(&state,view.state,total,fills){
                Ok(v)=>v,
                Err(_)=>return blocked(client,tenant,env,&c,st,"venue execution history is incomplete or inconsistent with the independent total",now).await,
            };
            let response=Canonical::<kanz_oms_venue::proto::venue::v1::QueryOrderResponse>::decode_with_budget(view.response.as_ref().ok_or(Error::HistoryIncomplete)?.wire(),Budget::RecoveryHistory)?;
            let quantity = response
                .message_wire("executed_quantity")?
                .ok_or(Error::HistoryIncomplete)?;
            let frozen = history::freeze_wire(&c.order_id, &mapping, &fills, &quantity, now)?;
            let mut next = c.clone();
            next.mapping_version = mapping;
            next.status = "investigating".into();
            next.reason.clear();
            let record = handlers::status(env, &next, st, now)?;
            match repository::freeze(client, tenant, &c, st, &frozen, &record).await {
                Ok(()) | Err(store::Error::Conflict) => {}
                Err(e) => return Err(e.into()),
            }
        } else {
            match prepared(client,tenant,env,&c,now).await {
                Ok(())=>return Ok(c.order_id),
                Err(preparation::ProjectionError::Block)=>return blocked(client,tenant,env,&c,st,"verified execution cannot be represented in the order aggregate; operator investigation required",now).await,
                Err(preparation::ProjectionError::Error(Error::Conflict))=>{},
                Err(preparation::ProjectionError::Error(Error::PostedHistoryUnknown|Error::EvidenceConflict|Error::FeeRevision|Error::HistoryIncomplete))=>return blocked(client,tenant,env,&c,st,"booked execution evidence is incomplete or differs from venue history; an authorized correction is required",now).await,
                Err(preparation::ProjectionError::Error(e))=>return Err(e),
            }
        }
        c = repository::load_case(client, tenant, &c.id).await?;
    }
    Err(Error::Conflict)
}

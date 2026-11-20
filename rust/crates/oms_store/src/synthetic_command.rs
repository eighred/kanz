//! Isolated OMS command owner for the disposable XSIM venue.
use super::{
    cas, claim, commit, enqueue, execution_key, insert_order, journal, load_in, recovery,
    same_execution, scope_existing, Error, Loaded, Record,
};
use kanz_decimal_compat::{compare, in_domain, is_positive};
use kanz_order_core::proto::{common::v1::Decimal, order::v1::*};
use kanz_order_core::{aggregate, Canonical};
use prost::Message;
use prost_types::Timestamp;
use sha2::{Digest, Sha256};
use std::time::SystemTime;
use tokio_postgres::{Client, Transaction};

const RUNTIME: &str = "kanz_synthetic_oms_runtime";
const ADAPTER: &str = "kanz_synthetic_oms_adapter";
const VENUE: &str = "XSIM";
const ACCOUNT: &str = "synthetic-spot";
const INSTRUMENT: &str = "SYNTH-USD";

pub struct Scope<'a> {
    pub tenant: &'a str,
    pub principal: &'a str,
    pub portfolio: &'a str,
}
pub struct Receipt {
    pub code: &'static str,
    pub order: Loaded,
    pub lineage: Lineage,
}

#[derive(Debug)]
pub enum Refusal {
    Invalid,
    Unauthorized,
    Controls(&'static str),
    Conflict,
    NotFound,
    State,
    Store(Error),
}
impl Refusal {
    pub fn code(&self) -> &'static str {
        match self {
            Self::Invalid => "INVALID",
            Self::Unauthorized => "UNAUTHORIZED",
            Self::Controls(_) => "CONTROL_DENIED",
            Self::Conflict => "CONFLICT",
            Self::NotFound => "NOT_FOUND",
            Self::State => "INVALID_STATE",
            Self::Store(Error::CommitUnconfirmed) => "COMMIT_UNCONFIRMED",
            Self::Store(_) => "STORE_UNAVAILABLE",
        }
    }
}
impl From<Error> for Refusal {
    fn from(e: Error) -> Self {
        match e {
            Error::UnsafeRole => Self::Unauthorized,
            Error::NotFound => Self::NotFound,
            Error::Conflict | Error::Exists | Error::IdentityConflict => Self::Conflict,
            Error::InvalidInput => Self::Invalid,
            e => Self::Store(e),
        }
    }
}
impl From<tokio_postgres::Error> for Refusal {
    fn from(e: tokio_postgres::Error) -> Self {
        Self::Store(e.into())
    }
}
type Result<T> = std::result::Result<T, Refusal>;
fn identifier(s: &str) -> bool {
    !s.is_empty() && s.len() <= 256 && s.bytes().all(|b| b.is_ascii_graphic())
}
fn order_id(s: &str) -> bool {
    s.len() == 32
        && s.bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}
fn positive(d: Option<&Decimal>) -> bool {
    in_domain(d) && is_positive(d)
}

async fn begin<'a>(
    client: &'a mut Client,
    s: &Scope<'_>,
    roles: &[&str],
) -> Result<Transaction<'a>> {
    if ![s.tenant, s.principal, s.portfolio]
        .into_iter()
        .all(identifier)
    {
        return Err(Refusal::Invalid);
    }
    let tx = client.transaction().await?;
    // Resolve all canonical relations in reviewed schemas, including the RLS preflight.
    tx.batch_execute("SET LOCAL search_path=pg_catalog,public,pg_temp; SET LOCAL statement_timeout='5s'; SET LOCAL lock_timeout='3s'").await?;
    scope_existing(&tx, s.tenant).await?;
    let row=tx.query_one("SELECT current_user::text, current_database()='kanz_synthetic_oms' AND current_user=session_user AND NOT has_table_privilege(current_user,'synthetic_oms_evidence','INSERT,UPDATE,DELETE,TRUNCATE') AND NOT has_any_column_privilege(current_user,'synthetic_oms_evidence','INSERT,UPDATE') AND (SELECT count(*)=3 FROM pg_class WHERE oid IN ('synthetic_oms_evidence'::regclass,'synthetic_oms_commands'::regclass,'synthetic_oms_observations'::regclass) AND relrowsecurity AND relforcerowsecurity AND NOT pg_has_role(current_user,relowner,'MEMBER'))", &[]).await?;
    if !row.get::<_, bool>(1) || !roles.contains(&row.get::<_, String>(0).as_str()) {
        return Err(Refusal::Unauthorized);
    }
    Ok(tx)
}

async fn observed_now(tx: &Transaction<'_>) -> Result<Timestamp> {
    let at: SystemTime = tx.query_one("SELECT clock_timestamp()", &[]).await?.get(0);
    Ok(Timestamp::from(at))
}
pub struct Lineage {
    pub event_id: String,
    pub trace_context: String,
}
// The entitlement producer captures the gateway's root envelope here. The
// idempotency key is caller-controlled and never an event or trace identity.
impl Lineage {
    async fn load(tx: &Transaction<'_>, id: &str) -> Result<Self> {
        let row=tx.query_opt(
            "SELECT e.source_id,e.trace_context FROM synthetic_oms_commands c
             JOIN synthetic_oms_evidence e USING(tenant_id,principal_id,portfolio_id,order_id,command_sha256)
             WHERE c.order_id=$1 AND e.kind='entitlement' AND e.source_id=ANY(c.evidence_ids)", &[&id]
        ).await?.ok_or(Refusal::Controls("MISSING_GATEWAY_LINEAGE"))?;
        let event_id: String = row.get(0);
        let trace_context: String = row.get(1);
        if !identifier(&event_id) || !traceparent(&trace_context) {
            return Err(Refusal::Controls("INVALID_GATEWAY_LINEAGE"));
        }
        Ok(Self {
            event_id,
            trace_context,
        })
    }
    fn fact(
        &self,
        scope: &Scope<'_>,
        id: &str,
        event: &str,
        name: &str,
        at: &Timestamp,
        cause: &str,
        payload: Vec<u8>,
    ) -> Result<Record> {
        let subject = format!("order.order.{event}");
        Ok(Record {
            tenant: scope.tenant.into(),
            partition_key: id.into(),
            subject: subject.clone(),
            event_type: subject,
            event_class: 1,
            schema_version: 1,
            domain: "order".into(),
            payload_schema_ref: format!("order.v1.{name}:1"),
            event_time: recovery::time(at)?,
            correlation_id: self.event_id.clone(),
            causation_id: cause.into(),
            trace_context: self.trace_context.clone(),
            payload,
        })
    }
}

fn traceparent(value: &str) -> bool {
    if value.is_empty() {
        return true;
    }
    fn hex(s: &[u8]) -> bool {
        s.iter()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(b))
    }
    let b = value.as_bytes();
    b.len() == 55
        && &b[..3] == b"00-"
        && b[35] == b'-'
        && b[52] == b'-'
        && hex(&b[3..35])
        && b[3..35].iter().any(|c| *c != b'0')
        && hex(&b[36..52])
        && b[36..52].iter().any(|c| *c != b'0')
        && hex(&b[53..55])
}
async fn controls(
    tx: &Transaction<'_>,
    scope: &Scope<'_>,
    id: &str,
    hash: &[u8],
) -> Result<Vec<String>> {
    let rows = tx
        .query(
            "SELECT source_id,kind,decision,
                effective_at<=observed_at AND observed_at<=statement_timestamp() AS observed,
                expires_at>statement_timestamp() AS valid,
                effective_at>=statement_timestamp()-interval '30 seconds' AS fresh
         FROM synthetic_oms_evidence
         WHERE principal_id=$1 AND portfolio_id=$2 AND order_id=$3 AND command_sha256=$4
           AND instrument_id='SYNTH-USD' ORDER BY kind",
            &[&scope.principal, &scope.portfolio, &id, &hash],
        )
        .await?;
    if rows.len() != 5 {
        return Err(Refusal::Controls("MISSING_CONTROL_EVIDENCE"));
    }
    for row in &rows {
        match row.get::<_, &str>(2) {
            "ALLOW" => {}
            "DENY" => return Err(Refusal::Controls("POLICY_DENIED")),
            _ => return Err(Refusal::Controls("POLICY_UNKNOWN")),
        }
        if !row.get::<_, bool>(3) {
            return Err(Refusal::Controls("INVALID_EVIDENCE_TIME"));
        }
        if !row.get::<_, bool>(4) {
            return Err(Refusal::Controls("EXPIRED_CONTROL_EVIDENCE"));
        }
        if row.get::<_, &str>(1) == "market" && !row.get::<_, bool>(5) {
            return Err(Refusal::Controls("STALE_MARKET"));
        }
    }
    Ok(rows.iter().map(|r| r.get(0)).collect())
}
fn admitted_command(scope: &Scope<'_>, cmd: &SubmitOrder, now: &Timestamp) -> Result<()> {
    let meta = cmd.metadata.as_ref().ok_or(Refusal::Unauthorized)?;
    if !scope.principal.starts_with("user:")
        || scope.principal.len() <= 5
        || meta.issuer != scope.principal
        || meta.target_id != cmd.order_id
        || cmd.portfolio_id != scope.portfolio
        || !meta
            .principal_portfolios
            .iter()
            .any(|p| p == scope.portfolio)
    {
        return Err(Refusal::Unauthorized);
    }
    if let Some(deadline) = meta.valid_until {
        if recovery::time(&deadline)? <= recovery::time(now)? {
            return Err(Refusal::Controls("COMMAND_EXPIRED"));
        }
    }
    if !order_id(&cmd.order_id)
        || cmd.venue != VENUE
        || cmd.instrument_id != INSTRUMENT
        || !matches!(Side::try_from(cmd.side), Ok(Side::Buy | Side::Sell))
        || cmd.time_in_force != TimeInForce::Gtc as i32
        || cmd.order_type != OrderType::Limit as i32
        || cmd.stop_price.is_some()
        || cmd.expire_at.is_some()
        || cmd.execution_schedule.is_some()
        || !cmd.parent_order_id.is_empty()
        || cmd.margin_mode != 0
        || cmd.leverage.is_some()
        || !positive(cmd.limit_price.as_ref())
        || !positive(cmd.quantity.as_ref())
    {
        return Err(Refusal::Invalid);
    }
    Ok(())
}
async fn replay(
    tx: &Transaction<'_>,
    s: &Scope<'_>,
    id: &str,
    key: &str,
    hash: &[u8],
) -> Result<Option<Receipt>> {
    let rows=tx.query("SELECT order_id,idempotency_key,principal_id,portfolio_id,command_sha256 FROM synthetic_oms_commands WHERE order_id=$1 OR idempotency_key=$2",&[&id,&key]).await?;
    if rows.is_empty() {
        return Ok(None);
    }
    if rows
        .iter()
        .any(|r| r.get::<_, String>(2) != s.principal || r.get::<_, String>(3) != s.portfolio)
    {
        return Err(Refusal::NotFound);
    }
    let r = &rows[0];
    if rows.len() != 1
        || r.get::<_, String>(0) != id
        || r.get::<_, String>(1) != key
        || r.get::<_, Vec<u8>>(4) != hash
    {
        return Err(Refusal::Conflict);
    }
    Ok(Some(Receipt {
        code: "DUPLICATE",
        lineage: Lineage::load(tx, id).await?,
        order: load_in(tx, id).await?,
    }))
}
pub async fn submit(client: &mut Client, s: &Scope<'_>, key: &str, wire: &[u8]) -> Result<Receipt> {
    if !identifier(key) || wire.is_empty() || wire.len() > 65536 {
        return Err(Refusal::Invalid);
    }
    let cmd = Canonical::<SubmitOrder>::decode(wire).map_err(|_| Refusal::Invalid)?;
    // Reject unsupported or ambiguous fields at this deliberately narrow ingress.
    if cmd.value().encode_to_vec() != wire {
        return Err(Refusal::Invalid);
    }
    let hash = Sha256::digest(wire).to_vec();
    let id = &cmd.value().order_id;
    let tx = begin(client, s, &[RUNTIME]).await?;
    if let Some(r) = replay(&tx, s, id, key, &hash).await? {
        return Ok(r);
    }
    let now = observed_now(&tx).await?;
    admitted_command(s, cmd.value(), &now)?;
    let evidence = controls(&tx, s, id, &hash).await?;
    let inserted=tx.execute("INSERT INTO synthetic_oms_commands(order_id,idempotency_key,principal_id,portfolio_id,command_sha256,command,evidence_ids) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING", &[id,&key,&s.principal,&s.portfolio,&hash,&wire,&evidence]).await?;
    if inserted == 0 {
        return replay(&tx, s, id, key, &hash)
            .await?
            .ok_or(Refusal::Conflict);
    }
    let mut st = aggregate::accept(&cmd, now)
        .map_err(|_| Refusal::Invalid)?
        .value()
        .clone();
    st.venue_account_id = ACCOUNT.into();
    let lineage = Lineage::load(&tx, id).await?;
    let record = lineage.fact(
        s,
        id,
        "accepted",
        "OrderAccepted",
        &now,
        &lineage.event_id,
        OrderAccepted {
            order_id: id.clone(),
            state: Some(st.clone()),
        }
        .encode_to_vec(),
    )?;
    insert_order(&tx, &st, &st.encode_to_vec()).await?;
    enqueue(&tx, &[record]).await?;
    let order = load_in(&tx, id).await?;
    commit(tx).await?;
    Ok(Receipt {
        code: "ACCEPTED",
        lineage,
        order,
    })
}
pub async fn get(client: &mut Client, s: &Scope<'_>, id: &str) -> Result<Receipt> {
    if !order_id(id) {
        return Err(Refusal::Invalid);
    }
    let tx = begin(client, s, &[RUNTIME, ADAPTER]).await?;
    if tx.query_opt("SELECT 1 FROM synthetic_oms_commands WHERE order_id=$1 AND portfolio_id=$2 AND (principal_id=$3 OR (current_user='kanz_synthetic_oms_adapter' AND $3='spiffe://kanz.internal/ns/kanz-services/sa/oms'))",&[&id,&s.portfolio,&s.principal]).await?.is_none(){return Err(Refusal::NotFound)}
    Ok(Receipt {
        code: "FOUND",
        lineage: Lineage::load(&tx, id).await?,
        order: load_in(&tx, id).await?,
    })
}

pub struct Observation<'a> {
    pub operation: &'a str,
    pub id: &'a str,
    pub seconds: i64,
    pub nanos: i32,
    pub body: &'a [u8],
}
impl Observation<'_> {
    fn digest(&self) -> Vec<u8> {
        let mut hash = Sha256::new();
        for p in [
            self.operation.as_bytes(),
            self.body,
            &self.seconds.to_be_bytes(),
            &self.nanos.to_be_bytes(),
        ] {
            hash.update((p.len() as u64).to_be_bytes());
            hash.update(p);
        }
        hash.finalize().to_vec()
    }
    fn is_claim(&self) -> bool {
        matches!(self.operation, "dispatch" | "cancel_pending")
    }
}
struct Reduction {
    code: &'static str,
    phase: String,
    state: Option<OrderState>,
    record: Option<Record>,
}
impl Reduction {
    fn new(code: &'static str, phase: &str) -> Self {
        Self {
            code,
            phase: phase.into(),
            state: None,
            record: None,
        }
    }
}
async fn fill(
    lineage: &Lineage,
    tx: &Transaction<'_>,
    s: &Scope<'_>,
    st: &Canonical<OrderState>,
    phase: &str,
    event: &Observation<'_>,
    at: Timestamp,
    now: Timestamp,
) -> Result<Reduction> {
    let fill = Canonical::<Fill>::decode(event.body).map_err(|_| Refusal::Invalid)?;
    let f = fill.value();
    let order = st.value();
    if f.order_id != order.order_id
        || f.venue != VENUE
        || f.venue_account_id != ACCOUNT
        || f.instrument_id != INSTRUMENT
        || f.side != order.side
        || f.executed_at != Some(at)
        || !identifier(&f.fill_id)
        || !identifier(&f.venue_execution_id)
        || f.recovery.is_some()
        || !positive(f.price.as_ref())
        || !positive(f.quantity.as_ref())
        || f.fee.as_ref().is_some_and(|fee| {
            fee.currency_code != "USD"
                || fee
                    .amount
                    .is_none_or(|d| d.coefficient < 0 || !in_domain(Some(&d)))
        })
        || f.encode_to_vec() != event.body
    {
        return Err(Refusal::Invalid);
    }
    let price = compare(f.price.as_ref(), order.limit_price.as_ref());
    if (order.side == Side::Buy as i32 && price.is_gt())
        || (order.side == Side::Sell as i32 && price.is_lt())
    {
        return Err(Refusal::Conflict);
    }
    let matches=tx.query("SELECT fill FROM order_executions WHERE (venue=$1 AND venue_account_id=$2 AND instrument_id=$3 AND venue_execution_id=$4) OR (order_id=$5 AND fill_id=$6)",&[&f.venue,&f.venue_account_id,&f.instrument_id,&f.venue_execution_id,&f.order_id,&f.fill_id]).await?;
    if !matches.is_empty() {
        if matches.len() != 1 {
            return Err(Refusal::Conflict);
        }
        let raw = matches[0].get::<_, Vec<u8>>(0);
        let prior = Fill::decode(raw.as_slice()).map_err(|_| Refusal::Conflict)?;
        if !same_execution(&prior, f) {
            return Err(Refusal::Conflict);
        }
        return Ok(Reduction::new("DUPLICATE", phase));
    }
    if !matches!(phase, "working" | "cancel_pending" | "unknown_cancel") {
        return Err(Refusal::State);
    }
    let mut next = aggregate::apply_fill(st, &fill, now)
        .map_err(|_| Refusal::Conflict)?
        .value()
        .clone();
    let complete = next.status == OrderStatus::Filled as i32;
    if complete {
        next.quarantine = None;
    }
    let id = &order.order_id;
    let record = if complete {
        lineage.fact(
            s,
            id,
            "filled",
            "OrderFilled",
            &at,
            event.id,
            OrderFilled {
                order_id: id.clone(),
                fill: Some(f.clone()),
                state: Some(next.clone()),
            }
            .encode_to_vec(),
        )?
    } else {
        lineage.fact(
            s,
            id,
            "partially_filled",
            "OrderPartiallyFilled",
            &at,
            event.id,
            OrderPartiallyFilled {
                order_id: id.clone(),
                fill: Some(f.clone()),
                state: Some(next.clone()),
            }
            .encode_to_vec(),
        )?
    };
    if claim(tx, id, &execution_key(f), &f.fill_id).await? != 1 {
        return Err(Refusal::Conflict);
    }
    journal(tx, id, &f.fill_id, f, event.body).await?;
    let mut result = Reduction::new("APPLIED", if complete { "filled" } else { phase });
    result.state = Some(next);
    result.record = Some(record);
    Ok(result)
}
pub async fn observe(
    client: &mut Client,
    s: &Scope<'_>,
    id: &str,
    event: &Observation<'_>,
) -> Result<Receipt> {
    if s.principal != "spiffe://kanz.internal/ns/kanz-services/sa/oms" {
        return Err(Refusal::Unauthorized);
    }
    if !order_id(id) || !identifier(event.id) || event.body.len() > 65536 {
        return Err(Refusal::Invalid);
    }
    let tx = begin(client, s, &[ADAPTER]).await?;
    let row=tx.query_opt("SELECT phase,principal_id,command_sha256,command FROM synthetic_oms_commands WHERE order_id=$1 AND portfolio_id=$2 FOR UPDATE",&[&id,&s.portfolio]).await?.ok_or(Refusal::NotFound)?;
    let phase: String = row.get(0);
    let lineage = Lineage::load(&tx, id).await?;
    let order = load_in(&tx, id).await?;
    let digest = event.digest();
    let recorded=tx.query_opt("SELECT operation,digest FROM synthetic_oms_observations WHERE order_id=$1 AND event_id=$2",&[&id,&event.id]).await?;
    if let Some(prior) = recorded {
        if prior.get::<_, String>(0) != event.operation
            || (!event.is_claim() && prior.get::<_, Vec<u8>>(1) != digest)
        {
            return Err(Refusal::Conflict);
        }
        return Ok(Receipt {
            lineage,
            code: if event.is_claim() {
                "RECONCILE_REQUIRED"
            } else {
                "DUPLICATE"
            },
            order,
        });
    }
    let now = observed_now(&tx).await?;
    let at = Timestamp {
        seconds: event.seconds,
        nanos: event.nanos,
    };
    if recovery::time(&at)? > recovery::time(&now)? {
        return Err(Refusal::Invalid);
    }
    let st = Canonical::<OrderState>::decode(&order.state).map_err(|_| Refusal::Invalid)?;
    if st.value().portfolio_id != s.portfolio
        || st.value().venue != VENUE
        || st.value().venue_account_id != ACCOUNT
        || st.value().instrument_id != INSTRUMENT
    {
        return Err(Refusal::State);
    }
    let result = match event.operation {
        "dispatch" if phase == "accepted" => {
            let principal: String = row.get(1);
            let owner = Scope {
                principal: &principal,
                ..*s
            };
            let command: Vec<u8> = row.get(3);
            let cmd = SubmitOrder::decode(command.as_slice()).map_err(|_| Refusal::Invalid)?;
            admitted_command(&owner, &cmd, &now)?;
            controls(&tx, &owner, id, &row.get::<_, Vec<u8>>(2)).await?;
            Reduction::new("DISPATCH_ONCE", "dispatch_claimed")
        }
        "dispatch" => Reduction::new("RECONCILE_REQUIRED", &phase),
        "cancel_pending" if phase == "working" => Reduction::new("CANCEL_ONCE", "cancel_pending"),
        "cancel_pending"
            if matches!(
                phase.as_str(),
                "cancel_pending" | "unknown_cancel" | "filled" | "cancelled"
            ) =>
        {
            Reduction::new("RECONCILE_REQUIRED", &phase)
        }
        "ack"
            if event.body == id.as_bytes()
                && matches!(phase.as_str(), "dispatch_claimed" | "unknown_execute") =>
        {
            let mut state = aggregate::route(&st, now)
                .map_err(|_| Refusal::State)?
                .value()
                .clone();
            state.venue_ack_at = Some(at);
            state.quarantine = None;
            let record = lineage.fact(
                s,
                id,
                "routed",
                "OrderRouted",
                &at,
                event.id,
                OrderRouted {
                    order_id: id.into(),
                    venue: VENUE.into(),
                    venue_order_id: id.into(),
                }
                .encode_to_vec(),
            )?;
            let mut result = Reduction::new("APPLIED", "working");
            result.state = Some(state);
            result.record = Some(record);
            result
        }
        "ack" if event.body == id.as_bytes() && st.value().venue_ack_at.is_some() => {
            Reduction::new("DUPLICATE", &phase)
        }
        "timeout"
            if matches!(
                phase.as_str(),
                "dispatch_claimed" | "cancel_pending" | "unknown_execute" | "unknown_cancel"
            ) =>
        {
            let mut state = st.value().clone();
            state.quarantine.get_or_insert(OrderQuarantine {
                at: Some(at),
                reason: "synthetic venue outcome unknown".into(),
                last_query_at: None,
            });
            let mut result = Reduction::new(
                "UNKNOWN",
                if matches!(phase.as_str(), "dispatch_claimed" | "unknown_execute") {
                    "unknown_execute"
                } else {
                    "unknown_cancel"
                },
            );
            if state != *st.value() {
                result.state = Some(state)
            }
            result
        }
        "fill" => fill(&lineage, &tx, s, &st, &phase, event, at, now).await?,
        "cancelled"
            if matches!(
                phase.as_str(),
                "cancel_pending" | "unknown_cancel" | "cancelled"
            ) =>
        {
            let cumulative = Decimal::decode(event.body).map_err(|_| Refusal::Invalid)?;
            if !in_domain(Some(&cumulative))
                || cumulative.coefficient < 0
                || st.value().filled_quantity.is_none()
                || compare(Some(&cumulative), st.value().filled_quantity.as_ref()).is_ne()
            {
                return Err(Refusal::Conflict);
            }
            if phase == "cancelled" {
                Reduction::new("DUPLICATE", &phase)
            } else {
                let mut state = aggregate::cancel(&st, now)
                    .map_err(|_| Refusal::State)?
                    .value()
                    .clone();
                state.quarantine = None;
                let record = lineage.fact(
                    s,
                    id,
                    "cancelled",
                    "OrderCancelled",
                    &at,
                    event.id,
                    OrderCancelled {
                        order_id: id.into(),
                        cancelled_quantity: state.leaves_quantity,
                    }
                    .encode_to_vec(),
                )?;
                let mut result = Reduction::new("APPLIED", "cancelled");
                result.state = Some(state);
                result.record = Some(record);
                result
            }
        }
        _ => return Err(Refusal::State),
    };
    if result.phase != phase {
        tx.execute(
            "UPDATE synthetic_oms_commands SET phase=$2 WHERE order_id=$1",
            &[&id, &result.phase],
        )
        .await?;
    }
    if let Some(state) = result.state {
        cas(&tx, &state, &state.encode_to_vec(), order.version).await?;
    }
    if let Some(record) = result.record {
        enqueue(&tx, &[record]).await?;
    }
    tx.execute("INSERT INTO synthetic_oms_observations(order_id,event_id,operation,digest,effective_seconds,effective_nanos,observation) VALUES ($1,$2,$3,$4,$5,$6,$7)",&[&id,&event.id,&event.operation,&digest,&event.seconds,&event.nanos,&event.body]).await?;
    let order = load_in(&tx, id).await?;
    commit(tx).await?;
    Ok(Receipt {
        code: result.code,
        lineage,
        order,
    })
}

#[cfg(test)]
mod synthetic_tests {
    use super::traceparent;

    #[test]
    fn gateway_trace_is_bounded_version_zero_with_nonzero_identifiers() {
        let good = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";
        assert!(traceparent(good));
        assert!(traceparent(""));
        for invalid in [
            good.to_uppercase(),
            good.replace("00-4", "ff-4"),
            good.replace("-01", "-zz"),
            good.replace("00f067aa0ba902b7", "0000000000000000"),
            good.replace(
                "4bf92f3577b34da6a3ce929d0e0e4736",
                "00000000000000000000000000000000",
            ),
            format!("{good}\n"),
            format!("{good}extra"),
            good.replace("-", "/"),
        ] {
            assert!(!traceparent(&invalid));
        }
    }
}

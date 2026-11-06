//! Disposable test adapter only. No service, production configuration or release image.
mod relay_support;
use base64::{engine::general_purpose::STANDARD, Engine};
use kanz_oms_fold_shadow::proto::order::v1::Fill;
use kanz_oms_store::{
    applied_fills, create, fee_subject, fee_wire::FillWire, identity, load, outbox, queries, save,
    Record,
};
use prost::Message;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::{
    io::{Read, Write},
    time::{Duration, UNIX_EPOCH},
};
use tokio_postgres::{config::Host, NoTls};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Input {
    mode: String,
    #[serde(default)]
    relay_hold_ms: u64,
    #[serde(default)]
    base_currency: String,
    #[serde(default)]
    relay_stop_after_ms: u64,
    #[serde(default)]
    proposal: Option<ProposalInput>,
    #[serde(default)]
    at: Option<Clock>,
    #[serde(default)]
    acknowledgement: String,
    #[serde(default)]
    book: String,
    #[serde(default)]
    case_version: i64,
    #[serde(default)]
    history_digest: String,
    #[serde(default)]
    missing: Vec<String>,
    #[serde(default)]
    posted: Vec<String>,
    #[serde(default)]
    rollback: bool,
    #[serde(default)]
    statuses: Vec<i32>,
    #[serde(default)]
    parent_id: String,
    #[serde(default)]
    portfolio_id: String,
    #[serde(default)]
    limit: i64,
    #[serde(default)]
    order_id: String,
    #[serde(default)]
    tenant: String,
    #[serde(default)]
    schema: String,
    #[serde(default)]
    state: String,
    #[serde(default)]
    expected_version: i64,
    #[serde(default)]
    fill_id: String,
    #[serde(default)]
    records: Vec<Fact>,
    #[serde(default)]
    fill: String,
    #[serde(default)]
    other: String,
}
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Clock {
    seconds: i64,
    nanos: u32,
}
impl Clock {
    fn time(&self) -> Result<std::time::SystemTime, ()> {
        if !(-62135596800..=253402300799).contains(&self.seconds) || self.nanos >= 1_000_000_000 {
            return Err(());
        }
        let base = if self.seconds < 0 {
            UNIX_EPOCH.checked_sub(Duration::from_secs(self.seconds.unsigned_abs()))
        } else {
            UNIX_EPOCH.checked_add(Duration::from_secs(self.seconds as u64))
        };
        base.and_then(|v| v.checked_add(Duration::from_nanos(self.nanos.into())))
            .ok_or(())
    }
    fn from_time(value: std::time::SystemTime) -> Result<Self, ()> {
        match value.duration_since(UNIX_EPOCH) {
            Ok(d) => Ok(Self {
                seconds: d.as_secs().try_into().map_err(|_| ())?,
                nanos: d.subsec_nanos(),
            }),
            Err(e) => {
                let d = e.duration();
                let seconds: i64 = d.as_secs().try_into().map_err(|_| ())?;
                Ok(if d.subsec_nanos() == 0 {
                    Self {
                        seconds: -seconds,
                        nanos: 0,
                    }
                } else {
                    Self {
                        seconds: -seconds - 1,
                        nanos: 1_000_000_000 - d.subsec_nanos(),
                    }
                })
            }
        }
    }
}
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct ProposalInput {
    id: String,
    subject: String,
    portfolio_id: String,
    act: String,
    proposer: String,
    digest: String,
    command: String,
    created_at: Clock,
    expires_at: Clock,
    #[serde(default)]
    approver: String,
    #[serde(default)]
    decided_at: Option<Clock>,
    #[serde(default)]
    refusal_reason: String,
    #[serde(default)]
    refused_by: String,
    #[serde(default)]
    refused_at: Option<Clock>,
    #[serde(default)]
    expiry_announced_at: Option<Clock>,
}
impl ProposalInput {
    fn proposal(self) -> Result<kanz_oms_store::proposals::Proposal, ()> {
        Ok(kanz_oms_store::proposals::Proposal {
            id: self.id,
            subject: self.subject,
            portfolio_id: self.portfolio_id,
            act: self.act,
            proposer: self.proposer,
            digest: self.digest,
            command: bytes(&self.command)?,
            created_at: self.created_at.time()?,
            expires_at: self.expires_at.time()?,
            approver: self.approver,
            decided_at: self.decided_at.map(|v| v.time()).transpose()?,
            refusal_reason: self.refusal_reason,
            refused_by: self.refused_by,
            refused_at: self.refused_at.map(|v| v.time()).transpose()?,
            expiry_announced_at: self.expiry_announced_at.map(|v| v.time()).transpose()?,
        })
    }
    fn from_proposal(p: kanz_oms_store::proposals::Proposal) -> Result<Self, ()> {
        Ok(Self {
            id: p.id,
            subject: p.subject,
            portfolio_id: p.portfolio_id,
            act: p.act,
            proposer: p.proposer,
            digest: p.digest,
            command: STANDARD.encode(p.command),
            created_at: Clock::from_time(p.created_at)?,
            expires_at: Clock::from_time(p.expires_at)?,
            approver: p.approver,
            decided_at: p.decided_at.map(Clock::from_time).transpose()?,
            refusal_reason: p.refusal_reason,
            refused_by: p.refused_by,
            refused_at: p.refused_at.map(Clock::from_time).transpose()?,
            expiry_announced_at: p.expiry_announced_at.map(Clock::from_time).transpose()?,
        })
    }
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fact {
    tenant: String,
    partition_key: String,
    subject: String,
    event_type: String,
    event_class: i32,
    schema_version: i32,
    domain: String,
    payload_schema_ref: String,
    event_seconds: u64,
    event_nanos: u32,
    correlation_id: String,
    causation_id: String,
    trace_context: String,
    payload: String,
}
fn bytes(v: &str) -> Result<Vec<u8>, ()> {
    STANDARD.decode(v).map_err(|_| ())
}
fn record(r: Fact) -> Result<Record, ()> {
    if r.event_nanos >= 1_000_000_000 {
        return Err(());
    }
    Ok(Record {
        tenant: r.tenant,
        partition_key: r.partition_key,
        subject: r.subject,
        event_type: r.event_type,
        event_class: r.event_class,
        schema_version: r.schema_version,
        domain: r.domain,
        payload_schema_ref: r.payload_schema_ref,
        event_time: UNIX_EPOCH
            .checked_add(Duration::new(r.event_seconds, r.event_nanos))
            .ok_or(())?,
        correlation_id: r.correlation_id,
        causation_id: r.causation_id,
        trace_context: r.trace_context,
        payload: bytes(&r.payload)?,
    })
}
async fn run(input: Input) -> Result<Value, ()> {
    if input.mode == "relay-frame" {
        if input.records.len() != 1 {
            return Err(());
        }
        let mut framer = kanz_oms_store::relay::Framer::new(
            "oms/relay-fixture".into(),
            option_env!("KANZ_BUILD_VERSION").unwrap_or_default().into(),
        )
        .map_err(|_| ())?;
        let record = record(input.records.into_iter().next().ok_or(())?)?;
        return Ok(match framer.frame(&record) {
            Ok(frame) => json!({"status":"framed","state":STANDARD.encode(frame.body)}),
            Err(_) => json!({"status":"InvalidRecord"}),
        });
    }
    if matches!(
        input.mode.as_str(),
        "fee-digest" | "fee-terms" | "fee-delta"
    ) {
        let raw = bytes(&input.fill)?;
        let other = bytes(&input.other)?;
        let result: Result<Value, kanz_oms_store::Error> = (|| {
            let revised = FillWire::decode(&raw)?;
            match input.mode.as_str() {
                "fee-digest" => Ok(
                    json!({"status":kanz_oms_store::fee_revision::economic_digest(revised.value())?}),
                ),
                "fee-terms" => Ok(
                    json!({"status":"terms","state":STANDARD.encode(kanz_oms_store::fee_revision::terms(&revised)?.journal_bytes()?)}),
                ),
                _ => {
                    let previous = FillWire::decode(&other)?;
                    Ok(
                        json!({"status":"delta","delta":kanz_oms_store::fee_revision::approved_delta(&previous,&revised)?.to_string()}),
                    )
                }
            }
        })();
        return Ok(match result {
            Ok(v) => v,
            Err(e) => json!({"status":e.to_string()}),
        });
    }
    if input.mode == "fee-wire" {
        let raw = bytes(&input.fill)?;
        let other = bytes(&input.other)?;
        let result = (|| {
            let a = FillWire::decode(&raw)?;
            let b = FillWire::decode(&other)?;
            Ok::<_, kanz_oms_store::Error>((a.equivalent(&b), a.journal_bytes()?))
        })();
        return Ok(match result {
            Ok((same, canonical)) => {
                json!({"status":same.to_string(),"state":STANDARD.encode(canonical)})
            }
            Err(e) => json!({"status":e.to_string()}),
        });
    }
    if input.mode == "fee-subject" {
        return Ok(
            json!({"status":fee_subject::normalize(&input.fill),"state":STANDARD.encode(fee_subject::normalize(&input.other))}),
        );
    }
    if input.mode == "fee-unicode" {
        return Ok(json!({"status":fee_subject::scalar_digest()}));
    }
    if input.mode == "identity" {
        let fill = Fill::decode(bytes(&input.fill)?.as_slice()).map_err(|_| ())?;
        return Ok(json!({"status":identity::execution_key(&fill)}));
    }
    if input.mode == "same" {
        let a = Fill::decode(bytes(&input.fill)?.as_slice()).map_err(|_| ())?;
        let b = Fill::decode(bytes(&input.other)?.as_slice()).map_err(|_| ())?;
        return Ok(json!({"status":identity::same_execution(&a, &b).to_string()}));
    }
    if !matches!(
        input.mode.as_str(),
        "save"
            | "position-apply"
            | "position-snapshot"
            | "position-projector"
            | "relay-once"
            | "relay-flush"
            | "relay-run"
            | "create"
            | "load"
            | "applied-fills"
            | "list"
            | "list-by-status"
            | "list-by-parent"
            | "list-by-portfolio"
            | "fee-revision"
            | "recovery"
            | "recovery-ack"
            | "outbox-keys"
            | "outbox-pending"
            | "outbox-published"
            | "outbox-failed"
            | "outbox-age"
            | "outbox-lock"
            | "outbox-cancel"
            | "outbox-cancel-fallback"
            | "proposal-put"
            | "proposal-get"
            | "proposal-claim"
            | "proposal-refusal"
            | "proposal-pending"
            | "proposal-expired"
            | "proposal-announce"
    ) || !input.schema.starts_with("oms_store_")
        || input.schema.len() > 63
        || !input
            .schema
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_')
    {
        return Err(());
    }
    let config: tokio_postgres::Config = std::env::var("TEST_POSTGRES_URL")
        .map_err(|_| ())?
        .parse()
        .map_err(|_| ())?;
    if !local_config(&config) {
        return Err(());
    }
    let (mut client, connection) = config.connect(NoTls).await.map_err(|_| ())?;
    let driver = tokio::spawn(async move { connection.await.is_ok() });
    if matches!(
        input.mode.as_str(),
        "outbox-lock" | "outbox-cancel" | "outbox-cancel-fallback"
    ) {
        if !(1..=5000).contains(&input.limit) {
            driver.abort();
            return Err(());
        }
        client
            .query_one(
                "SELECT set_config('search_path',$1,false),set_config('application_name',$1,false)",
                &[&input.schema],
            )
            .await
            .map_err(|_| ())?;
        let fallback = input.mode == "outbox-cancel-fallback";
        let session = outbox::Session::new(client, driver, move |token| async move {
            if fallback {
                std::future::pending().await
            } else {
                token.cancel_query(NoTls).await
            }
        });
        let mut acquire = Box::pin(outbox::lock_key(
            session,
            &input.tenant,
            &input.order_id,
            input.rollback,
        ));
        if input.mode == "outbox-cancel" || fallback {
            // The cancellation clock starts only after PostgreSQL reports this
            // session actively waiting for the advisory lock. Otherwise a fast
            // CancelRequest can precede query execution and prove nothing.
            tokio::select! {
                _ = &mut acquire => return Err(()),
                observed = observe_blocked(&config, &input.schema) => observed?,
            }
            return Ok(
                match tokio::time::timeout(Duration::from_millis(input.limit as u64), acquire).await
                {
                    Err(_) => {
                        let canceled_at = std::time::Instant::now();
                        // Check while this process is still alive: process exit must
                        // not conceal a leaked blocked session after future drop.
                        let (observer, connection) = config.connect(NoTls).await.map_err(|_| ())?;
                        let observer_driver = tokio::spawn(async move { connection.await.is_ok() });
                        let absent = async {
                        for _ in 0..if fallback {700} else {100} {
                            let count:i64 = observer.query_one("SELECT count(*) FROM pg_stat_activity WHERE application_name=$1", &[&input.schema]).await.map_err(|_| ())?.try_get(0).map_err(|_| ())?;
                            if count==0 { return Ok::<_,()>(true); }
                            tokio::time::sleep(Duration::from_millis(10)).await;
                        }
                        Ok(false)
                    }.await;
                        drop(observer);
                        observer_driver.abort();
                        if !absent?
                            || (!fallback && canceled_at.elapsed() >= Duration::from_secs(2))
                        {
                            return Err(());
                        }
                        json!({"status":"canceled"})
                    }
                    Ok(Ok(Some(lock))) => {
                        drop(lock);
                        json!({"status":"unexpected-lock"})
                    }
                    Ok(Ok(None)) => json!({"status":"contended"}),
                    Ok(Err(e)) => json!({"status":e.to_string()}),
                },
            );
        }
        return Ok(match acquire.await {
            Ok(Some(lock)) => {
                tokio::time::sleep(Duration::from_millis(input.limit as u64)).await;
                match lock.release().await {
                    Ok(session) => {
                        drop(session);
                        json!({"status":"released"})
                    }
                    Err(e) => json!({"status":e.to_string()}),
                }
            }
            Ok(None) => json!({"status":"contended"}),
            Err(e) => json!({"status":e.to_string()}),
        });
    }
    let result = async {
        client
            .query_one(
                "SELECT set_config('search_path',$1,false)",
                &[&input.schema],
            )
            .await
            .map_err(|_| ())?;
        if input.mode=="position-projector" { return relay_support::run(config.clone(),&mut client,&input).await; }
        if input.mode.starts_with("position-") {
            use kanz_oms_store::position;
            let at=input.at.as_ref().ok_or(())?.time()?;
            if input.mode=="position-snapshot" {
                return Ok(match position::snapshot(&mut client,&input.tenant,&input.portfolio_id,at,&input.base_currency).await {
                    Ok(value)=>json!({"status":"snapshot","state":STANDARD.encode(value.encode_to_vec())}),Err(e)=>json!({"status":e.to_string()}),
                });
            }
            let fill=bytes(&input.fill)?;
            let templates=input.records.into_iter().map(record).collect::<Result<Vec<_>,_>>()?;
            let result=position::apply(&mut client,&input.tenant,&input.portfolio_id,&fill,at,&input.base_currency,|applied|{
                if input.other=="announce-fail" { return Err(kanz_oms_store::Error::InvalidInput); }
                if !input.other.is_empty() || (!templates.is_empty() && !(2..=3).contains(&templates.len())) {return Err(kanz_oms_store::Error::InvalidInput);}
                let mut records=templates;
                for (r,state) in records.iter_mut().take(2).zip([&applied.aggregate,&applied.venue]) {
                    r.payload=state.encode_to_vec();
                    let stamp=state.as_of.as_ref().ok_or(kanz_oms_store::Error::InvalidInput)?;
                    r.event_time=Clock{seconds:stamp.seconds,nanos:stamp.nanos.try_into().map_err(|_|kanz_oms_store::Error::InvalidInput)?}.time().map_err(|_|kanz_oms_store::Error::InvalidInput)?;
                }
                Ok(records)
            }).await;
            return Ok(match result {Ok(value)=>json!({"status":"applied","venue":STANDARD.encode(value.venue.encode_to_vec()),"aggregate":STANDARD.encode(value.aggregate.encode_to_vec())}),Err(e)=>json!({"status":e.to_string()})});
        }
        if input.mode.starts_with("relay-") { return relay_support::run(config.clone(),&mut client,&input).await; }
        if input.mode.starts_with("proposal-") {
            use kanz_oms_store::proposals;
            let at=match input.at.as_ref(){Some(at)=>at.time()?,None if matches!(input.mode.as_str(),"proposal-get"|"proposal-put")=>UNIX_EPOCH,None=>return Err(())};
            let records=input.records.into_iter().map(record).collect::<Result<Vec<_>,_>>()?;
            let result:Result<Value,kanz_oms_store::Error>=async {
                Ok(match input.mode.as_str(){
                    "proposal-put"=>{let p=input.proposal.ok_or(kanz_oms_store::Error::InvalidInput)?.proposal().map_err(|_|kanz_oms_store::Error::InvalidInput)?;proposals::put(&mut client,&input.tenant,&p,&records).await?;json!({"status":"committed"})},
                    "proposal-get"=>match proposals::get(&mut client,&input.tenant,&input.order_id).await?{Some(p)=>json!({"status":"proposal","proposal":ProposalInput::from_proposal(p).map_err(|_|kanz_oms_store::Error::InvalidInput)?}),None=>json!({"status":"missing"})},
                    "proposal-pending"|"proposal-expired"=>{let rows=if input.mode=="proposal-pending"{proposals::pending(&mut client,&input.tenant,at).await?}else{proposals::expired_unannounced(&mut client,&input.tenant,at,input.limit).await?};json!({"status":"proposals","proposals":rows.into_iter().map(ProposalInput::from_proposal).collect::<Result<Vec<_>,_>>().map_err(|_|kanz_oms_store::Error::InvalidInput)?})},
                    "proposal-claim"=>json!({"status":"decision","won":proposals::claim(&mut client,&input.tenant,&input.order_id,&input.other,at,&records).await?}),
                    "proposal-refusal"=>json!({"status":"decision","won":proposals::record_refusal(&mut client,&input.tenant,&input.order_id,&input.other,&input.fill,at).await?}),
                    "proposal-announce"=>json!({"status":"decision","won":proposals::announce_expiry(&mut client,&input.tenant,&input.order_id,at,&records).await?}),
                    _=>return Err(kanz_oms_store::Error::InvalidInput),
                })
            }.await;
            return Ok(match result{Ok(v)=>v,Err(e)=>json!({"status":e.to_string()})});
        }
        if input.mode=="recovery-ack" {
            use kanz_oms_fold_shadow::proto::order::v1::ExecutionRecoveryApplied;
            use kanz_oms_store::recovery_ack::{acknowledge_recovery,Book};
            let raw=bytes(&input.acknowledgement)?;
            if raw.len()>4096 || input.records.len()!=1 { return Err(()); }
            let ack=ExecutionRecoveryApplied::decode(raw.as_slice()).map_err(|_|())?;
            let book=match input.book.as_str(){"ledger"=>Book::Ledger,"position"=>Book::Position,_=>return Err(())};
            let corrected=record(input.records.into_iter().next().ok_or(())?)?;
            return Ok(match acknowledge_recovery(&mut client,&input.tenant,&ack,book,&corrected).await {
                Ok(())=>json!({"status":"committed"}),Err(e)=>json!({"status":e.to_string()}),
            });
        }
        if input.mode.starts_with("outbox-") {
            let result: Result<Value, kanz_oms_store::Error> = async {
                Ok(match input.mode.as_str() {
                    "outbox-keys" => json!({"status":"keys","claims":outbox::pending_keys(&mut client,&input.tenant,input.limit).await?}),
                    "outbox-pending" => {
                        let rows=outbox::pending(&mut client,&input.tenant,&input.order_id,input.limit).await?;
                        let mut output=Vec::new();
                        for p in rows {
                            let r=p.record;
                            let time=r.event_time.duration_since(UNIX_EPOCH).map_err(|_|kanz_oms_store::Error::InvalidInput)?;
                            output.push(json!({"id":p.id,"attempts":p.attempts,"last_error":p.last_error,"tenant":r.tenant,"partition_key":r.partition_key,"subject":r.subject,"event_type":r.event_type,"event_class":r.event_class,"schema_version":r.schema_version,"domain":r.domain,"payload_schema_ref":r.payload_schema_ref,"event_seconds":time.as_secs(),"event_nanos":time.subsec_nanos(),"correlation_id":r.correlation_id,"causation_id":r.causation_id,"trace_context":r.trace_context,"payload":STANDARD.encode(r.payload)}));
                        }
                        json!({"status":"pending","pending":output})
                    },
                    "outbox-published" => {outbox::mark_published(&mut client,&input.tenant,input.expected_version).await?;json!({"status":"marked"})},
                    "outbox-failed" => {outbox::mark_failed(&mut client,&input.tenant,input.expected_version,Some(&input.other)).await?;json!({"status":"marked"})},
                    "outbox-age" => {let now=UNIX_EPOCH.checked_add(Duration::from_secs(input.expected_version.try_into().map_err(|_|kanz_oms_store::Error::InvalidInput)?)).ok_or(kanz_oms_store::Error::InvalidInput)?;json!({"status":"age","age_ns":outbox::oldest_pending_age(&mut client,&input.tenant,now).await?.map(|a|a.as_nanos() as u64)})},
                    _ => return Err(kanz_oms_store::Error::InvalidInput),
                })
            }.await;
            return Ok(match result {Ok(value)=>value,Err(e)=>json!({"status":e.to_string()})});
        }
        if input.mode=="recovery" {
            let batch=kanz_oms_store::recovery::PreparedRecovery {
                state:bytes(&input.state)?,order_version:input.expected_version,case_version:input.case_version,
                history_digest:input.history_digest,missing:input.missing,
                posted:input.posted.iter().map(|raw| FillWire::decode(&bytes(raw)?).map_err(|_|())).collect::<Result<Vec<_>,_>>()?,
                records:input.records.into_iter().map(record).collect::<Result<Vec<_>,_>>()?,
            };
            return Ok(match kanz_oms_store::recovery::commit_prepared_recovery(&mut client,&input.tenant,&batch).await {
                Ok(())=>json!({"status":"committed"}),Err(e)=>json!({"status":e.to_string()}),
            });
        }
        if input.mode=="fee-revision" {
            let original=FillWire::decode(&bytes(&input.other)?).map_err(|_|())?;
            let revised=FillWire::decode(&bytes(&input.fill)?).map_err(|_|())?;
            let state=bytes(&input.state)?;
            let records=input.records.into_iter().map(record).collect::<Result<Vec<_>,_>>()?;
            let mut tx=client.transaction().await.map_err(|_|())?;
            let result=async {
                let mut locked=kanz_oms_store::fee_journal::cas_order(&mut tx,&input.tenant,&state,input.expected_version).await?;
                let revision=locked.record_fee_revision(&original,&revised).await?;
                locked.enqueue(&records).await?;
                Ok::<_,kanz_oms_store::Error>(revision)
            }.await;
            return Ok(match result {
                Err(e)=>{tx.rollback().await.map_err(|_|())?;json!({"status":e.to_string()})},
                Ok(revision)=>{
                    if input.rollback {tx.rollback().await.map_err(|_|())?;json!({"status":"rolled_back","delta":revision.delta.to_string(),"fresh":revision.fresh})}
                    else {match tx.commit().await {
                        Ok(())=>json!({"status":"committed","delta":revision.delta.to_string(),"fresh":revision.fresh}),
                        Err(_)=>json!({"status":"CommitUnconfirmed"}),
                    }}
                },
            });
        }
        if input.mode=="list-by-portfolio" {
            return Ok(match queries::list_by_portfolio(&mut client,&input.tenant,&input.portfolio_id,input.limit).await {
                Ok(page)=>json!({"status":"orders","states":page.states.iter().map(|s|STANDARD.encode(s)).collect::<Vec<_>>(),"unindexed":page.unindexed}),
                Err(e)=>json!({"status":e.to_string()}),
            });
        }
        if matches!(input.mode.as_str(),"list"|"list-by-status"|"list-by-parent") {
            let result=match input.mode.as_str() {
                "list"=>queries::list(&mut client,&input.tenant).await,
                "list-by-status"=>queries::list_by_status(&mut client,&input.tenant,&input.statuses).await,
                _=>queries::list_by_parent(&mut client,&input.tenant,&input.parent_id).await,
            };
            return Ok(match result {
                Ok(rows)=>json!({"status":"orders","states":rows.iter().map(|s|STANDARD.encode(s)).collect::<Vec<_>>()}),
                Err(e)=>json!({"status":e.to_string()}),
            });
        }
        if input.mode=="load" {
            return Ok(match load(&mut client,&input.tenant,&input.order_id).await {
                Ok(row)=>json!({"status":"loaded","state":STANDARD.encode(&row.state),"db_version":row.version}),
                Err(e)=>json!({"status":e.to_string()}),
            });
        }
        if input.mode=="applied-fills" {
            return Ok(match applied_fills(&mut client,&input.tenant,&input.order_id).await {
                Ok(claims)=>json!({"status":"claims","claims":claims}),Err(e)=>json!({"status":e.to_string()}),
            });
        }
        let records=input.records.into_iter().map(record).collect::<Result<Vec<_>,_>>()?;
        let state=bytes(&input.state)?;
        let result=if input.mode=="create" {create(&mut client,&input.tenant,&state,&records).await}
            else {save(&mut client,&input.tenant,&state,input.expected_version,&records,&input.fill_id).await};
        Ok(json!({"status":match result {Ok(())=>"committed".into(),Err(e)=>e.to_string()}}))
    }
    .await;
    drop(client);
    driver.abort();
    let _ = driver.await;
    result
}
async fn observe_blocked(config: &tokio_postgres::Config, schema: &str) -> Result<(), ()> {
    let (observer, connection) = config.connect(NoTls).await.map_err(|_| ())?;
    let driver = tokio::spawn(async move { connection.await.is_ok() });
    let result = async {
        for _ in 0..500 {
            let waiting:bool=observer.query_one("SELECT EXISTS(SELECT 1 FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid WHERE a.application_name=$1 AND l.locktype='advisory' AND NOT l.granted)", &[&schema]).await.map_err(|_|())?.try_get(0).map_err(|_|())?;
            if waiting { return Ok(()); }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        Err(())
    }.await;
    drop(observer);
    driver.abort();
    result
}
fn local_config(config: &tokio_postgres::Config) -> bool {
    !config.get_hosts().is_empty()
        && config.get_hostaddrs().is_empty()
        && config
            .get_hosts()
            .iter()
            .all(|h| matches!(h,Host::Tcp(s) if s=="127.0.0.1"||s=="localhost"||s=="::1"))
}
#[tokio::main]
async fn main() {
    let result = async {
        if std::env::args().len() != 1 {
            return Err(());
        }
        let version = option_env!("KANZ_BUILD_VERSION")
            .filter(|s| !s.is_empty())
            .ok_or(())?;
        let mut raw = Vec::new();
        std::io::stdin()
            .take(2 * 1024 * 1024 + 1)
            .read_to_end(&mut raw)
            .map_err(|_| ())?;
        if raw.len() > 2 * 1024 * 1024 {
            return Err(());
        }
        let input = serde_json::from_slice(&raw).map_err(|_| ())?;
        let mut output = tokio::time::timeout(Duration::from_secs(20), run(input))
            .await
            .map_err(|_| ())??;
        output["version"] = json!(version);
        let encoded = output.to_string();
        if encoded.len() > 2 * 1024 * 1024 {
            return Err(());
        }
        std::io::stdout()
            .write_all(encoded.as_bytes())
            .map_err(|_| ())
    }
    .await;
    if result.is_err() {
        eprintln!("OMS store fixture refused or outcome unconfirmed");
        std::process::exit(1)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn destination_is_loopback_only() {
        for s in ["host=localhost", "host=127.0.0.1", "host=::1"] {
            assert!(local_config(&s.parse().unwrap()));
        }
        for s in [
            "host=localhost hostaddr=203.0.113.1",
            "host=localhost hostaddr=127.0.0.1",
            "host=example.invalid",
            "host=/tmp",
            "user=fixture",
        ] {
            assert!(!local_config(&s.parse().unwrap()));
        }
    }
}

//! Disposable test adapter only. No service, production configuration or release image.
use base64::{engine::general_purpose::STANDARD, Engine};
use kanz_oms_fold_shadow::proto::order::v1::Fill;
use kanz_oms_store::{
    applied_fills, create, fee_subject, fee_wire::FillWire, identity, load, queries, save, Record,
};
use prost::Message;
use serde::Deserialize;
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
            | "create"
            | "load"
            | "applied-fills"
            | "list"
            | "list-by-status"
            | "list-by-parent"
            | "list-by-portfolio"
            | "fee-revision"
            | "recovery"
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
    let result = async {
        client
            .query_one(
                "SELECT set_config('search_path',$1,false)",
                &[&input.schema],
            )
            .await
            .map_err(|_| ())?;
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

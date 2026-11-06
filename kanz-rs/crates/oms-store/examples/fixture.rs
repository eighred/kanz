//! Disposable test adapter only. No service, production configuration or release image.
use base64::{engine::general_purpose::STANDARD, Engine};
use kanz_oms_fold_shadow::proto::order::v1::Fill;
use kanz_oms_store::{applied_fills, create, identity, load, save, Record};
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
        "save" | "create" | "load" | "applied-fills"
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

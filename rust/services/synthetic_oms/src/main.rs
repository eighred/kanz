#![forbid(unsafe_code)]
use base64::{engine::general_purpose::STANDARD, Engine};
use kanz_oms_store::synthetic_command::{self as owner, Observation, Scope};
use serde::Deserialize;
use serde_json::{json, Value};
use std::{io::Read, str::FromStr, time::Duration};
use tokio_postgres::{config::Host, Config, NoTls};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    version: u32,
    op: String,
    tenant: String,
    principal: String,
    portfolio: String,
    idempotency_key: Option<String>,
    command_b64: Option<String>,
    order_id: Option<String>,
    event_id: Option<String>,
    effective_seconds: Option<i64>,
    effective_nanos: Option<i32>,
    venue_order_id: Option<String>,
    fill_b64: Option<String>,
    executed_quantity_b64: Option<String>,
}
fn decode(raw: Option<&String>) -> Result<Vec<u8>, &'static str> {
    STANDARD
        .decode(raw.ok_or("INVALID")?)
        .map_err(|_| "INVALID")
}
fn synthetic_login(name: &str) -> bool {
    ["kanz_synthetic_oms_runtime", "kanz_synthetic_oms_adapter"]
        .into_iter()
        .any(|base| {
            name == base
                || name
                    .strip_prefix(base)
                    .and_then(|suffix| suffix.strip_prefix("_g"))
                    .is_some_and(|n| {
                        !n.starts_with('0')
                            && n.parse::<u64>().is_ok_and(|v| v > 0 && n == v.to_string())
                    })
        })
}
async fn execute(r: Request) -> Result<Value, &'static str> {
    if r.version != 1 {
        return Err("UNSUPPORTED_VERSION");
    }
    let dsn = std::env::var("SYNTHETIC_OMS_DATABASE_URL").map_err(|_| "STORE_UNAVAILABLE")?;
    let config = Config::from_str(&dsn).map_err(|_| "STORE_UNAVAILABLE")?;
    if config.get_dbname() != Some("kanz_synthetic_oms")
        || config.get_hosts().len() != 1
        || !config.get_hostaddrs().is_empty()
        || !config.get_user().is_some_and(synthetic_login)
    {
        return Err("UNAUTHORIZED");
    }
    match &config.get_hosts()[0] {
        Host::Tcp(host) if host == "127.0.0.1" || host == "::1" => {}
        #[cfg(unix)]
        Host::Unix(_) => {}
        _ => return Err("UNAUTHORIZED"),
    }
    let (mut client, connection) = config
        .connect(NoTls)
        .await
        .map_err(|_| "STORE_UNAVAILABLE")?;
    tokio::spawn(async move {
        let _ = connection.await;
    });
    let s = Scope {
        tenant: &r.tenant,
        principal: &r.principal,
        portfolio: &r.portfolio,
    };
    let outcome = match r.op.as_str() {
        "submit" => {
            owner::submit(
                &mut client,
                &s,
                r.idempotency_key.as_deref().ok_or("INVALID")?,
                &decode(r.command_b64.as_ref())?,
            )
            .await
        }
        "get" => owner::get(&mut client, &s, r.order_id.as_deref().ok_or("INVALID")?).await,
        "cancel_funded" => {
            owner::cancel_funded(
                &mut client,
                &s,
                r.event_id.as_deref().ok_or("INVALID")?,
                &decode(r.command_b64.as_ref())?,
            )
            .await
        }
        "dispatch" | "ack" | "fill" | "timeout" | "cancel_pending" | "cancelled" => {
            let body = match r.op.as_str() {
                "ack" => r
                    .venue_order_id
                    .as_deref()
                    .ok_or("INVALID")?
                    .as_bytes()
                    .to_vec(),
                "fill" => decode(r.fill_b64.as_ref())?,
                "cancelled" => decode(r.executed_quantity_b64.as_ref())?,
                _ => vec![],
            };
            let event = Observation {
                operation: &r.op,
                id: r.event_id.as_deref().ok_or("INVALID")?,
                seconds: r.effective_seconds.ok_or("INVALID")?,
                nanos: r.effective_nanos.ok_or("INVALID")?,
                body: &body,
            };
            owner::observe(
                &mut client,
                &s,
                r.order_id.as_deref().ok_or("INVALID")?,
                &event,
            )
            .await
        }
        _ => return Err("INVALID"),
    };
    let receipt = match outcome {
        Ok(receipt) => receipt,
        Err(owner::Refusal::Controls(reason)) => {
            return Ok(json!({"ok":false,"code":"CONTROL_DENIED","reason":reason}))
        }
        Err(e) => return Err(e.code()),
    };
    Ok(
        json!({"ok":true,"code":receipt.code,"state_b64":STANDARD.encode(receipt.order.state),"order_version":receipt.order.version,"source_event_id":receipt.lineage.event_id,"trace_context":receipt.lineage.trace_context}),
    )
}
fn main() {
    let started = std::time::Instant::now();
    let result = (|| {
        let mut raw = Vec::new();
        std::io::stdin()
            .take(131073)
            .read_to_end(&mut raw)
            .map_err(|_| "INVALID")?;
        if raw.len() > 131072 {
            return Err("INVALID");
        }
        let r = serde_json::from_slice(&raw).map_err(|_| "INVALID")?;
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .map_err(|_| "STORE_UNAVAILABLE")?
            .block_on(async {
                tokio::time::timeout(Duration::from_secs(8), execute(r))
                    .await
                    .map_err(|_| "COMMIT_UNCONFIRMED")?
            })
    })();
    let reply = result.unwrap_or_else(|code| json!({"ok":false,"code":code}));
    eprintln!(
        "{}",
        json!({"service":"synthetic_oms","event":"command_result","code":reply["code"],"ok":reply["ok"],"elapsed_ms":started.elapsed().as_millis() as u64})
    );
    println!("{}", reply);
}

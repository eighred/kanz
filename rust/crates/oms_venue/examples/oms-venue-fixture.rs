//! Interactive synthetic qualification only. No exchange credentials or production configuration.
use base64::{engine::general_purpose::STANDARD, Engine};
use kanz_oms_venue::{Canonical, Client, Config, Requirements, ViewState};
use kanz_order_core::proto::order::v1::OrderState;
use serde::Deserialize;
use serde_json::{json, Value};
use std::time::Duration;
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tonic::Status;
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Init {
    endpoint: String,
    workload_socket: String,
    expected_peer: String,
    tenant: String,
    mic: String,
    account: String,
    require_verified_account: bool,
    require_order_types: bool,
    timeout_ms: u64,
    #[serde(default)]
    cancel_connect_after_ms: Option<u64>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Command {
    method: String,
    #[serde(default)]
    state: String,
    #[serde(default)]
    timeout_ms: u64,
}
async fn line(reader: &mut BufReader<tokio::io::Stdin>) -> Result<Vec<u8>, ()> {
    let mut raw = Vec::new();
    reader
        .take(2 * 1024 * 1024 + 1)
        .read_until(b'\n', &mut raw)
        .await
        .map_err(|_| ())?;
    if raw.len() > 2 * 1024 * 1024 || raw.last() != Some(&b'\n') {
        return Err(());
    }
    Ok(raw)
}
async fn output(mut value: Value) -> Result<(), ()> {
    value["version"] = json!(env!("KANZ_BUILD_VERSION"));
    let mut raw = serde_json::to_vec(&value).map_err(|_| ())?;
    raw.push(b'\n');
    let mut stdout = tokio::io::stdout();
    stdout.write_all(&raw).await.map_err(|_| ())?;
    stdout.flush().await.map_err(|_| ())
}
fn state(c: &Command) -> Result<Canonical<OrderState>, Status> {
    let raw = STANDARD
        .decode(&c.state)
        .map_err(|_| Status::invalid_argument("invalid state encoding"))?;
    Canonical::decode(&raw)
}
fn view(v: ViewState) -> &'static str {
    match v {
        ViewState::Indeterminate => "Indeterminate",
        ViewState::Unknown => "Unknown",
        ViewState::Working => "Working",
        ViewState::PartiallyFilled => "PartiallyFilled",
        ViewState::Filled => "Filled",
        ViewState::Rejected => "Rejected",
        ViewState::Cancelled => "Cancelled",
        ViewState::Expired => "Expired",
    }
}
async fn run() -> Result<(), ()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let mut input = BufReader::new(tokio::io::stdin());
    let init: Init = serde_json::from_slice(&line(&mut input).await?).map_err(|_| ())?;
    if init.timeout_ms == 0
        || init.timeout_ms > 30000
        || init
            .cancel_connect_after_ms
            .is_some_and(|n| n == 0 || n > init.timeout_ms)
    {
        return Err(());
    }
    // Deliberately restrict the executable fixture to local disposable services.
    let uri: http::Uri = init.endpoint.parse().map_err(|_| ())?;
    if !matches!(uri.host(), Some("127.0.0.1" | "localhost" | "[::1]")) {
        return Err(());
    }
    let version = env!("KANZ_BUILD_VERSION");
    let (stop_tx, stop_rx) = tokio::sync::watch::channel(false);
    let after = init.cancel_connect_after_ms.unwrap_or(init.timeout_ms);
    let timer = tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(after)).await;
        let _ = stop_tx.send(true);
    });
    let connected = Client::connect_until(
        Config {
            endpoint: init.endpoint,
            workload_socket: init.workload_socket,
            expected_peer: init.expected_peer,
            tenant: init.tenant,
        },
        stop_rx,
    )
    .await;
    timer.abort();
    let _ = timer.await;
    let mut client = match connected {
        Ok(client) => {
            output(json!({"status":"connected","version":version})).await?;
            Some(client)
        }
        Err(error) => {
            output(json!({"status":if init.cancel_connect_after_ms.is_some() && error.code()==tonic::Code::Cancelled {"cancelled"}else{"error"},"code":error.code() as i32,"version":version})).await?;
            None
        }
    };
    for _ in 0..128 {
        let command: Command = serde_json::from_slice(&line(&mut input).await?).map_err(|_| ())?;
        if command.method == "Close" {
            if let Some(client) = client.take() {
                client.close().await;
            }
            output(json!({"status":"ok","code":0,"sessions_closed":true})).await?;
            // Remain alive for independent socket/task closure observation before EOF.
            let mut byte = [0u8; 1];
            match tokio::time::timeout(Duration::from_secs(5), input.read(&mut byte)).await {
                Ok(Ok(0)) => return Ok(()),
                _ => return Err(()),
            }
        }
        let result=async {
            let client=client.as_mut().ok_or_else(||Status::unavailable("venue not connected"))?;
            let timeout=Duration::from_millis(command.timeout_ms);
            match command.method.as_str() {
                "Execute"=>{let response=client.execute_response(&state(&command)?,timeout).await?;
                    Ok(json!({"response":STANDARD.encode(response.wire()),"fills":response.fills()?.iter().map(|f|STANDARD.encode(f.wire())).collect::<Vec<_>>()}))},
                "CancelOrder"=>{client.cancel_order(&state(&command)?,timeout).await?;Ok(json!({"response":""}))},
                "Describe"=>{let response=client.describe(timeout).await?;Ok(json!({"response":STANDARD.encode(response.wire())}))},
                "ListInstruments"=>{let response=client.list_instruments(timeout).await?;Ok(json!({"response":STANDARD.encode(response.wire())}))},
                "QueryOrder"=>{let response=client.query_order(&state(&command)?,timeout).await?;
                    Ok(json!({"response":response.response.as_ref().map(|r|STANDARD.encode(r.wire())).unwrap_or_default(),"view":view(response.state),"fills":response.fills.iter().map(|f|STANDARD.encode(f.wire())).collect::<Vec<_>>()}))},
                "Register"=>{let result=client.register(&init.mic,&init.account,Requirements{verified_account:init.require_verified_account,declared_order_types:init.require_order_types},timeout).await?;
                    let(catalog,code)=match result.instruments {Ok(value)=>(STANDARD.encode(value.wire()),0),Err(code)=>(String::new(),code as i32)};
                    Ok(json!({"response":STANDARD.encode(result.identity.wire()),"catalog":catalog,"catalog_code":code}))},
                _=>Err(Status::invalid_argument("unknown fixture method")),
            }
        }.await;
        let value = match result {
            Ok(mut value) => {
                value["status"] = json!("ok");
                value["code"] = json!(0);
                value
            }
            Err(error) => json!({"status":"error","code":error.code() as i32}),
        };
        output(value).await?;
    }
    if let Some(client) = client {
        client.close().await;
    }
    Err(())
}
#[tokio::main]
async fn main() {
    if run().await.is_err() {
        eprintln!("venue fixture refused input or transport");
        std::process::exit(1);
    }
}

//! Disposable loopback qualifier; no deployment binary or production connector.
use base64::{engine::general_purpose::STANDARD, Engine};
use kanz_oms_recovery::{Config, Coordinator, Method};
use kanz_oms_store::{
    outbox::Session,
    relay::{Frame, Framer, PublishFailure, Publisher, Sessions, Task},
    Error,
};
use serde::Deserialize;
use serde_json::json;
use std::time::Duration;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt};
use tokio_postgres::{config::Host, NoTls};
const LINE: usize = 8 * 1024 * 1024;
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Init {
    endpoint: String,
    workload_socket: String,
    expected_peer: String,
    tenant: String,
    mic: String,
    account: String,
    portfolio: String,
    schema: String,
    timeout_ms: u64,
    #[serde(default)]
    session_delay_ms: u64,
    #[serde(default)]
    hold_after_ack: u64,
    #[serde(default)]
    ack_hold_ms: u64,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct At {
    seconds: i64,
    nanos: i32,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Command {
    method: String,
    #[serde(default)]
    envelope: String,
    #[serde(default)]
    payload: String,
    at: Option<At>,
    #[serde(default)]
    timeout_ms: u64,
}
#[derive(Clone)]
struct Connector {
    config: tokio_postgres::Config,
    schema: String,
    delay: Duration,
}
struct Driver(Option<tokio::task::JoinHandle<bool>>);
impl Drop for Driver {
    fn drop(&mut self) {
        if let Some(t) = &self.0 {
            t.abort();
        }
    }
}
impl Sessions for Connector {
    fn connect(&mut self) -> Task<'_, Result<Session, Error>> {
        Box::pin(async {
            tokio::time::sleep(self.delay).await;
            let (client, connection) = self.config.connect(NoTls).await?;
            let mut driver = Driver(Some(tokio::spawn(async move { connection.await.is_ok() })));
            let mut session = Session::new(
                client,
                driver.0.take().ok_or(Error::Database)?,
                |token| async move { token.cancel_query(NoTls).await },
            );
            let configured=session.client().query_one("SELECT set_config('search_path',$1,false),set_config('statement_timeout','30000',false)",&[&self.schema]).await;
            if configured.is_err() {
                session.close().await;
                return Err(Error::Database);
            }
            Ok(session)
        })
    }
}
struct Broker {
    context: async_nats::jetstream::Context,
    acknowledgements: u64,
    hold_after: u64,
    hold: Duration,
}
impl Publisher for Broker {
    fn publish<'a>(&'a mut self, frame: &'a Frame) -> Task<'a, Result<(), PublishFailure>> {
        Box::pin(async move {
            let mut headers = async_nats::HeaderMap::new();
            headers.insert("Nats-Msg-Id", frame.event_id.as_str());
            headers.insert("Kanz-Partition-Key", frame.partition_key.as_str());
            self.context
                .publish_with_headers(frame.subject.clone(), headers, frame.body.clone().into())
                .await
                .map_err(|_| PublishFailure::Broker)?
                .await
                .map_err(|_| PublishFailure::Broker)?;
            self.acknowledgements += 1;
            if self.hold_after != 0 && self.acknowledgements == self.hold_after {
                tokio::time::sleep(self.hold).await;
            }
            Ok(())
        })
    }
}
async fn line(reader: &mut tokio::io::BufReader<tokio::io::Stdin>) -> Result<Option<Vec<u8>>, ()> {
    let mut out = Vec::new();
    loop {
        let buf = reader.fill_buf().await.map_err(|_| ())?;
        if buf.is_empty() {
            return if out.is_empty() { Ok(None) } else { Err(()) };
        }
        let n = buf
            .iter()
            .position(|b| *b == b'\n')
            .map_or(buf.len(), |n| n + 1);
        if out.len().checked_add(n).is_none_or(|n| n > LINE) {
            return Err(());
        }
        let end = buf[n - 1] == b'\n';
        out.extend_from_slice(&buf[..n]);
        reader.consume(n);
        if end {
            return Ok(Some(out));
        }
    }
}
async fn report(version: &str, status: &str, error: &str, closed: bool) -> Result<(), ()> {
    let mut out = serde_json::to_vec(
        &json!({"version":version,"build_profile":if cfg!(debug_assertions) {"debug"} else {"release"},"status":status,"error":error,"sessions_closed":closed}),
    )
    .map_err(|_| ())?;
    if out.len() > 16384 {
        return Err(());
    }
    out.push(b'\n');
    let mut stdout = tokio::io::stdout();
    stdout.write_all(&out).await.map_err(|_| ())?;
    stdout.flush().await.map_err(|_| ())
}
fn deadline(ms: u64) -> Result<Duration, ()> {
    if ms == 0 || ms > 30000 {
        return Err(());
    }
    Ok(Duration::from_millis(ms))
}
async fn closed(observer: &tokio_postgres::Client, application: &str) -> Result<(), ()> {
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            let count: i64 = observer
                .query_one(
                    "SELECT count(*) FROM pg_stat_activity WHERE application_name=$1",
                    &[&application],
                )
                .await
                .map_err(|_| ())?
                .get(0);
            if count == 0 {
                return Ok(());
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .map_err(|_| ())?
}
async fn run() -> Result<(), ()> {
    let version = option_env!("KANZ_BUILD_VERSION").ok_or(())?;
    if version.len() != 40 || !version.bytes().all(|b| b.is_ascii_hexdigit()) {
        return Err(());
    }
    let mut reader = tokio::io::BufReader::new(tokio::io::stdin());
    let init: Init =
        serde_json::from_slice(&line(&mut reader).await?.ok_or(())?).map_err(|_| ())?;
    let uri: http::Uri = init.endpoint.parse().map_err(|_| ())?;
    if init.session_delay_ms > 2000
        || !matches!(init.hold_after_ack, 0 | 2)
        || init.ack_hold_ms > 2000
        || (init.hold_after_ack == 0) != (init.ack_hold_ms == 0)
        || uri.scheme_str() != Some("https")
        || !matches!(uri.host(), Some("127.0.0.1" | "[::1]"))
        || uri.authority().is_none_or(|a| a.as_str().contains('@'))
        || !init.schema.starts_with("oms_recovery_")
        || init.schema.len() > 63
        || !init
            .schema
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    {
        return Err(());
    }
    let mut pg: tokio_postgres::Config = std::env::var("TEST_POSTGRES_URL")
        .map_err(|_| ())?
        .parse()
        .map_err(|_| ())?;
    if !pg.get_hostaddrs().is_empty()
        || pg.get_hosts().is_empty()
        || !pg
            .get_hosts()
            .iter()
            .all(|h| matches!(h,Host::Tcp(host) if host=="127.0.0.1"||host=="::1"))
    {
        return Err(());
    }
    pg.connect_timeout(Duration::from_secs(5));
    let application = format!("oms-recovery-{}", std::process::id());
    let (observer, connection) = pg.connect(NoTls).await.map_err(|_| ())?;
    let observer_driver = Driver(Some(tokio::spawn(async move { connection.await.is_ok() })));
    let safe: bool = observer
        .query_one(
            "SELECT NOT rolsuper AND NOT rolbypassrls FROM pg_roles WHERE rolname=current_user",
            &[],
        )
        .await
        .map_err(|_| ())?
        .get(0);
    if !safe {
        return Err(());
    }
    pg.application_name(&application);
    let address = std::env::var("TEST_NATS_URL").map_err(|_| ())?;
    let port = address.strip_prefix("nats://127.0.0.1:").ok_or(())?;
    if !port.bytes().all(|b| b.is_ascii_digit()) || port.parse::<u16>().map_err(|_| ())? < 1024 {
        return Err(());
    }
    let nc = tokio::time::timeout(Duration::from_secs(5), async_nats::connect(address))
        .await
        .map_err(|_| ())?
        .map_err(|_| ())?;
    let config = Config {
        venue: kanz_oms_venue::Config {
            endpoint: init.endpoint,
            workload_socket: init.workload_socket,
            expected_peer: init.expected_peer,
            tenant: init.tenant,
        },
        portfolio: init.portfolio,
        mic: init.mic,
        account: init.account,
    };
    let mut owner = match Coordinator::connect(
        config,
        Connector {
            config: pg,
            schema: init.schema,
            delay: Duration::from_millis(init.session_delay_ms),
        },
        Broker {
            context: async_nats::jetstream::new(nc.clone()),
            acknowledgements: 0,
            hold_after: init.hold_after_ack,
            hold: Duration::from_millis(init.ack_hold_ms),
        },
        Framer::new("oms/recovery-fixture".into(), version.into()).map_err(|_| ())?,
        deadline(init.timeout_ms)?,
    )
    .await
    {
        Ok(owner) => owner,
        Err(e) => {
            report(version, "error", &format!("{e:?}"), true).await?;
            return Ok(());
        }
    };
    report(version, "connected", "", true).await?;
    for _ in 0..20000 {
        let Some(raw) = line(&mut reader).await? else {
            owner.close().await;
            return Ok(());
        };
        let command: Command = serde_json::from_slice(&raw).map_err(|_| ())?;
        if command.method == "Close" {
            owner.close().await;
            closed(&observer, &application).await?;
            nc.flush().await.map_err(|_| ())?;
            report(version, "closed", "", true).await?;
            // Remain alive so Go can observe transport/session closure before EOF.
            if line(&mut reader).await?.is_some() {
                return Err(());
            }
            drop(observer);
            drop(observer_driver);
            return Ok(());
        }
        let method = match command.method.as_str() {
            "Observation" => Method::Observation,
            "Ack" => Method::Ack,
            "Parked" => Method::Parked,
            _ => return Err(()),
        };
        let env = STANDARD.decode(command.envelope).map_err(|_| ())?;
        let payload = STANDARD.decode(command.payload).map_err(|_| ())?;
        let at = command.at.ok_or(())?;
        let result = owner
            .handle(
                method,
                &env,
                &payload,
                prost_types::Timestamp {
                    seconds: at.seconds,
                    nanos: at.nanos,
                },
                deadline(command.timeout_ms)?,
            )
            .await;
        closed(&observer, &application).await?;
        match result {
            Ok(()) => report(version, "ok", "", true).await?,
            Err(e) => report(version, "error", &format!("{e:?}"), true).await?,
        }
    }
    owner.close().await;
    Err(())
}
#[tokio::main]
async fn main() {
    let _ = rustls::crypto::ring::default_provider().install_default();
    if !matches!(
        tokio::time::timeout(Duration::from_secs(900), run()).await,
        Ok(Ok(()))
    ) {
        std::process::exit(1);
    }
}

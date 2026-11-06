//! Only this disposable fixture links NATS. The library has no broker connector.
use kanz_oms_store::{
    outbox::Session,
    relay::{Frame, Framer, PublishFailure, Publisher, Relay, Sessions, Stall, Task},
    Error,
};
use serde_json::{json, Value};
use std::{sync::atomic::Ordering, time::Duration};
use tokio_postgres::{Config, NoTls};

struct Connector {
    config: Config,
    schema: String,
}
struct ConnectingDriver(Option<tokio::task::JoinHandle<bool>>);
impl Drop for ConnectingDriver {
    fn drop(&mut self) {
        if let Some(driver) = self.0.as_ref() {
            driver.abort();
        }
    }
}
impl Sessions for Connector {
    fn connect(&mut self) -> Task<'_, Result<Session, Error>> {
        Box::pin(async {
            let (client, connection) = self.config.connect(NoTls).await?;
            let mut driver =
                ConnectingDriver(Some(tokio::spawn(async move { connection.await.is_ok() })));
            let configured=client.query_one("SELECT set_config('search_path',$1,false),set_config('statement_timeout','5000',false)",&[&self.schema]).await;
            if configured.is_err() {
                drop(client);
                if let Some(driver) = driver.0.take() {
                    driver.abort();
                    let _ = driver.await;
                }
                return Err(Error::Database);
            }
            let config = self.config.clone();
            let session = Session::new(
                client,
                driver.0.take().ok_or(Error::Database)?,
                move |token| async move { token.cancel_query(&config, NoTls).await },
            );
            Ok(session)
        })
    }
}
struct Broker {
    js: async_nats::jetstream::Context,
    mode: String,
    hold: Duration,
    first: bool,
}
impl Publisher for Broker {
    fn publish<'a>(&'a mut self, frame: &'a Frame) -> Task<'a, Result<(), PublishFailure>> {
        Box::pin(async move {
            let first = std::mem::replace(&mut self.first, false);
            if first && self.mode == "fail-first" {
                return Err(PublishFailure::Broker);
            }
            let mut headers = async_nats::HeaderMap::new();
            headers.insert("Nats-Msg-Id", frame.event_id.as_str());
            if !frame.partition_key.is_empty() {
                headers.insert("Kanz-Partition-Key", frame.partition_key.as_str());
            }
            self.js
                .publish_with_headers(frame.subject.clone(), headers, frame.body.clone().into())
                .await
                .map_err(|_| PublishFailure::Broker)?
                .await
                .map_err(|_| PublishFailure::Broker)?;
            if first && self.mode == "block-first" {
                tokio::time::sleep(self.hold).await;
            }
            Ok(())
        })
    }
}
pub async fn run(
    mut config: Config,
    client: &mut tokio_postgres::Client,
    input: &super::Input,
) -> Result<Value, ()> {
    let address = std::env::var("TEST_NATS_URL").map_err(|_| ())?;
    let port = address.strip_prefix("nats://127.0.0.1:").ok_or(())?;
    if !port.bytes().all(|b| b.is_ascii_digit())
        || port.parse::<u16>().map_err(|_| ())? < 1024
        || !matches!(input.other.as_str(), "" | "fail-first" | "block-first")
        || input.relay_hold_ms > 2000
        || input.relay_stop_after_ms > 2000
    {
        return Err(());
    }
    let nc = tokio::time::timeout(Duration::from_secs(5), async_nats::connect(address))
        .await
        .map_err(|_| ())?
        .map_err(|_| ())?;
    let application = format!("oms-relay-{}", uuid::Uuid::now_v7());
    config.application_name(&application);
    config.connect_timeout(Duration::from_secs(5));
    let mut relay = Relay::new(
        Broker {
            js: async_nats::jetstream::new(nc.clone()),
            mode: input.other.clone(),
            hold: Duration::from_millis(input.relay_hold_ms),
            first: true,
        },
        Connector {
            config,
            schema: input.schema.clone(),
        },
        Framer::new(
            "oms/relay-fixture".into(),
            option_env!("KANZ_BUILD_VERSION").unwrap_or_default().into(),
        )
        .map_err(|_| ())?,
        input.tenant.clone(),
        input.limit,
        Duration::from_millis(20),
    )
    .map_err(|_| ())?;
    let mut result = match input.mode.as_str() {
        "relay-flush" => match relay.flush(&input.order_id).await {
            Ok(out) => report(out.sent, out.stalled, None),
            Err(e) => report(e.sent, None, Some(e.error)),
        },
        "relay-once" => match relay.drain_once(client).await {
            Ok(sent) => report(sent, None, None),
            Err(e) => report(e.sent, None, Some(e.error)),
        },
        "relay-run" => {
            if input.relay_stop_after_ms == 0 {
                return Err(());
            }
            let stop = relay.stop_handle();
            let wait = Duration::from_millis(input.relay_stop_after_ms);
            let stopper = tokio::spawn(async move {
                tokio::time::sleep(wait).await;
                stop.store(true, Ordering::Release)
            });
            let mut sent = 0;
            let mut error = None;
            relay
                .run(client, |result| match result {
                    Ok(n) => sent += n,
                    Err(e) => {
                        sent += e.sent;
                        error = Some(e.error.to_string())
                    }
                })
                .await;
            stopper.await.map_err(|_| ())?;
            json!({"status":"drained","sent":sent,"stalled":"Stopped","error":error.unwrap_or_default()})
        }
        _ => return Err(()),
    };
    drop(relay);
    // Observe closure while this process is alive; process exit cannot provide
    // a false proof that owned drivers and their advisory locks were joined.
    tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            let count: i64 = client
                .query_one(
                    "SELECT count(*) FROM pg_stat_activity WHERE application_name=$1",
                    &[&application],
                )
                .await
                .map_err(|_| ())?
                .try_get(0)
                .map_err(|_| ())?;
            if count == 0 {
                return Ok::<(), ()>(());
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .map_err(|_| ())??;
    result["sessions_closed"] = json!(true);
    tokio::time::timeout(Duration::from_secs(5), nc.drain())
        .await
        .map_err(|_| ())?
        .map_err(|_| ())?;
    Ok(result)
}
fn report(sent: usize, stall: Option<Stall>, error: Option<Error>) -> Value {
    let stalled = match stall {
        None => "",
        Some(Stall::Contended) => "Contended",
        Some(Stall::FullBatch) => "FullBatch",
        Some(Stall::Stopped) => "Stopped",
        Some(Stall::Publication(PublishFailure::InvalidRecord)) => "InvalidRecord",
        Some(Stall::Publication(PublishFailure::Broker)) => "Broker",
    };
    json!({"status":if error.is_some(){"error"}else{"drained"},"sent":sent,"stalled":stalled,"error":error.map(|e|e.to_string()).unwrap_or_default()})
}

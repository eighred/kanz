#![forbid(unsafe_code)]
use kanz_binance_depth::{
    book::Book,
    publisher::{self, Framer},
    source::{self, Event},
};
use std::{
    env,
    error::Error,
    sync::{
        atomic::{AtomicBool, AtomicU64, Ordering},
        Arc, Mutex,
    },
    time::Duration,
};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::TcpListener,
    sync::{mpsc, watch},
    time::{interval, timeout},
};
type Result<T> = std::result::Result<T, Box<dyn Error + Send + Sync>>;

#[derive(Default)]
struct Health {
    ready: AtomicBool,
    connected: AtomicBool,
    publications: AtomicU64,
    validity: source::Validity,
    book_epoch: AtomicU64,
    venue_time: Mutex<Option<prost_types::Timestamp>>,
}
impl Health {
    fn is_ready(&self, wall: prost_types::Timestamp) -> bool {
        self.ready.load(Ordering::Relaxed)
            && self.connected.load(Ordering::Relaxed)
            && source::valid_epoch(&self.validity, self.book_epoch.load(Ordering::SeqCst))
            && self.venue_time.lock().is_ok_and(|time| fresh(*time, wall))
    }
}
struct Config {
    source: source::Config,
    tenant: String,
    producer: String,
    version: String,
    nats: String,
    socket: String,
    listen: String,
    development: bool,
}
fn required(name: &str) -> Result<String> {
    let value = env::var(name).map_err(|_| format!("{name} is required"))?;
    if value.trim().is_empty() {
        return Err(format!("{name} cannot be blank").into());
    }
    Ok(value)
}
fn producer_version(
    stamped: Option<&str>,
    development: bool,
    fallback: Option<String>,
) -> Result<String> {
    match stamped.filter(|value| !value.trim().is_empty()) {
        Some(value) => Ok(value.to_owned()),
        None if development => fallback
            .filter(|value| !value.trim().is_empty())
            .ok_or_else(|| {
                "BINANCE_DEPTH_VERSION required for an unstamped development build".into()
            }),
        None => Err("production binary requires a build-time KANZ_BUILD_VERSION".into()),
    }
}
impl Config {
    fn load() -> Result<Self> {
        let development = match env::var("BINANCE_DEPTH_DEVELOPMENT").as_deref() {
            Ok("true") => true,
            Ok("false") | Err(env::VarError::NotPresent) => false,
            _ => return Err("BINANCE_DEPTH_DEVELOPMENT must be true or false".into()),
        };
        let config = Self {
            source: source::Config {
                instrument_id: required("BINANCE_DEPTH_INSTRUMENT_ID")?,
                symbol: required("BINANCE_DEPTH_SYMBOL")?,
                mic: "BINANCE".into(),
                depth_limit: 1000,
                allow_local_test: development,
                ws_base: env::var("BINANCE_DEPTH_WS_BASE")
                    .unwrap_or_else(|_| "wss://stream.binance.com:9443".into()),
                rest_base: env::var("BINANCE_DEPTH_REST_BASE")
                    .unwrap_or_else(|_| "https://api.binance.com".into()),
            },
            tenant: required("BINANCE_DEPTH_TENANT")?,
            producer: required("BINANCE_DEPTH_SOURCE")?,
            version: producer_version(
                option_env!("KANZ_BUILD_VERSION"),
                development,
                env::var("BINANCE_DEPTH_VERSION").ok(),
            )?,
            nats: required("BINANCE_DEPTH_NATS_URL")?,
            socket: env::var("SPIFFE_ENDPOINT_SOCKET").unwrap_or_default(),
            listen: env::var("BINANCE_DEPTH_LISTEN").unwrap_or_else(|_| "0.0.0.0:8088".into()),
            development,
        };
        config.source.validate()?;
        let url = reqwest::Url::parse(&config.nats).map_err(|_| "invalid NATS address")?;
        if !url.username().is_empty()
            || url.password().is_some()
            || url.query().is_some()
            || url.fragment().is_some()
            || !matches!(url.scheme(), "nats" | "tls")
            || url.path() != "/" && !url.path().is_empty()
        {
            return Err(
                "NATS address must be a bare broker endpoint without embedded credentials".into(),
            );
        }
        if config.development {
            if !matches!(
                url.host_str(),
                Some("127.0.0.1" | "localhost" | "[::1]" | "::1")
            ) {
                return Err("development NATS must be loopback".into());
            }
        } else if config.socket.trim().is_empty() {
            return Err("SPIFFE_ENDPOINT_SOCKET required outside loopback development".into());
        }
        Ok(config)
    }
}

async fn connect(
    config: &Config,
    health: Arc<Health>,
) -> Result<(async_nats::Client, Option<spiffe::X509Source>)> {
    let observer = health.clone();
    let mut options = async_nats::ConnectOptions::new()
        .name(config.producer.clone())
        .connection_timeout(Duration::from_secs(5))
        .event_callback(move |event| {
            let observer = observer.clone();
            async move {
                let connected = matches!(event, async_nats::Event::Connected);
                if matches!(
                    event,
                    async_nats::Event::Connected
                        | async_nats::Event::Disconnected
                        | async_nats::Event::Closed
                ) {
                    observer.connected.store(connected, Ordering::Relaxed);
                    if !connected {
                        observer.ready.store(false, Ordering::Relaxed);
                    }
                }
            }
        });
    let identity = if !config.socket.is_empty() {
        let identity = timeout(
            Duration::from_secs(30),
            spiffe::X509Source::builder()
                .endpoint(&config.socket)
                .build(),
        )
        .await??;
        let tls = spiffe_rustls::mtls_client(identity.clone())
            .trust_domain_policy(spiffe_rustls::LocalOnly("kanz.internal".try_into()?))
            .authorize(spiffe_rustls::authorizer::exact([
                "spiffe://kanz.internal/ns/kanz-messaging/sa/nats",
            ])?)
            .build()?;
        options = options.require_tls(true).tls_client_config(tls);
        Some(identity)
    } else {
        None
    };
    let client = options
        .connect(&config.nats)
        .await
        .map_err(|_| "NATS connection failed")?;
    health.connected.store(true, Ordering::Relaxed);
    Ok((client, identity))
}

fn fresh(venue: Option<prost_types::Timestamp>, wall: prost_types::Timestamp) -> bool {
    let Some(venue) = venue else {
        return false;
    };
    let delta = i128::from(wall.seconds) * 1_000_000_000 + i128::from(wall.nanos)
        - i128::from(venue.seconds) * 1_000_000_000
        - i128::from(venue.nanos);
    (-1_000_000_000..=5_000_000_000).contains(&delta)
}

async fn operations(
    listener: TcpListener,
    health: Arc<Health>,
    mut stop: watch::Receiver<bool>,
) -> Result<()> {
    loop {
        let (mut socket, _) = tokio::select! {
            changed = stop.changed() => { let _ = changed; return Ok(()); }
            accepted = listener.accept() => accepted?,
        };
        // One bounded request at a time; the operations port exposes no data API.
        let _ = timeout(Duration::from_secs(1), async {
            let mut request = [0u8; 512];
            let count = socket.read(&mut request).await?;
            let line = std::str::from_utf8(&request[..count]).unwrap_or("").lines().next().unwrap_or("");
            let ready = health.is_ready(publisher::now());
            let (status, body) = if line.starts_with("GET /healthz ") { ("200 OK", "ok\n".into()) }
                else if line.starts_with("GET /readyz ") { if ready { ("200 OK", "ready\n".into()) } else { ("503 Service Unavailable", "not ready\n".into()) } }
                else if line.starts_with("GET /metrics ") { ("200 OK", format!("kanz_binance_depth_ready {}\nkanz_binance_depth_publications_total {}\n", u8::from(ready), health.publications.load(Ordering::Relaxed))) }
                else { ("404 Not Found", "not found\n".into()) };
            socket.write_all(format!("HTTP/1.1 {status}\r\nContent-Type: text/plain\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).as_bytes()).await
        }).await;
    }
}

async fn shutdown() -> Result<()> {
    #[cfg(unix)]
    {
        let mut terminate =
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;
        tokio::select! { result = tokio::signal::ctrl_c() => result?, _ = terminate.recv() => {} }
    }
    #[cfg(not(unix))]
    tokio::signal::ctrl_c().await?;
    Ok(())
}

async fn run(config: Config) -> Result<()> {
    let health = Arc::new(Health::default());
    let listener = TcpListener::bind(&config.listen).await?;
    let (client, identity) = connect(&config, health.clone()).await?;
    let js = async_nats::jetstream::new(client.clone());
    let mut framer = Framer::new(
        config.tenant,
        config.producer,
        config.version,
        config.source.instrument_id.clone(),
    )?;
    let mut book = Book::new(
        config.source.instrument_id.clone(),
        config.source.symbol.clone(),
        "BINANCE".into(),
    );
    let (stop, cancelled) = watch::channel(false);
    let (sender, mut updates) = mpsc::channel(8);
    let mut source_task = tokio::spawn(source::run(
        config.source,
        sender,
        cancelled.clone(),
        health.validity.clone(),
    ));
    let mut ops_task = tokio::spawn(operations(listener, health.clone(), cancelled));
    let mut ticker = interval(Duration::from_millis(250));
    // Keep the signal subscription alive across every feed/ticker iteration.
    let shutdown_signal = shutdown();
    tokio::pin!(shutdown_signal);
    let mut anchored = false;
    let mut book_epoch = 0;
    let result: Result<()> = loop {
        tokio::select! {
            signal = &mut shutdown_signal => break signal,
            ops = &mut ops_task => break match ops { Ok(Ok(())) => Err("operations server stopped".into()), Ok(Err(e)) => Err(e), Err(e) => Err(e.into()) },
            update = updates.recv() => match update {
                Some(Event::Reanchoring) => { anchored = false; health.ready.store(false, Ordering::Relaxed); }
                Some(Event::Snapshot(epoch, snapshot)) => {
                    if !source::valid_epoch(&health.validity, epoch) { continue; }
                    if book.apply_snapshot(&snapshot).is_err() { break Err("snapshot fold refused".into()); }
                    book_epoch = epoch; anchored = true;
                    health.book_epoch.store(epoch, Ordering::SeqCst);
                    if let Ok(mut time) = health.venue_time.lock() { *time = snapshot.event_time; }
                    else { break Err("health lock poisoned".into()); }
                }
                Some(Event::Delta(epoch, delta)) => {
                    if epoch != book_epoch || !source::valid_epoch(&health.validity, epoch) { continue; }
                    if book.apply_delta(&delta).is_err() { break Err("delta fold refused".into()); }
                    if let Ok(mut time) = health.venue_time.lock() { *time = book.snapshot(1).event_time; }
                    else { break Err("health lock poisoned".into()); }
                }
                None => break Err("depth source stopped".into()),
            },
            _ = ticker.tick() => {
                let snapshot = book.snapshot(100);
                let wall = publisher::now();
                if !anchored || !source::valid_epoch(&health.validity, book_epoch) || !fresh(snapshot.event_time, wall) || !health.connected.load(Ordering::Relaxed) {
                    health.ready.store(false, Ordering::Relaxed); continue;
                }
                if snapshot.bids.is_empty() && snapshot.asks.is_empty() { health.ready.store(false, Ordering::Relaxed); continue; }
                let frames = match framer.frames(&snapshot, wall) { Ok(frames) => frames, Err(error) => break Err(error) };
                if let Err(error) = publisher::publish(&js, frames, &health.validity, book_epoch).await { break Err(error); }
                health.publications.fetch_add(1, Ordering::Relaxed);
                health.ready.store(source::valid_epoch(&health.validity, book_epoch)
                    && fresh(snapshot.event_time, publisher::now())
                    && health.connected.load(Ordering::Relaxed), Ordering::Relaxed);
            },
        }
    };
    health.ready.store(false, Ordering::Relaxed);
    let _ = stop.send(true);
    // Publication runs inline above, so no transport is closed over a live ACK.
    if timeout(Duration::from_secs(15), &mut source_task)
        .await
        .is_err()
    {
        source_task.abort();
        let _ = source_task.await;
    }
    if !ops_task.is_finished()
        && timeout(Duration::from_secs(2), &mut ops_task)
            .await
            .is_err()
    {
        ops_task.abort();
        let _ = ops_task.await;
    }
    let drained = timeout(Duration::from_secs(5), client.drain()).await;
    drop(js);
    drop(client);
    if let Some(identity) = identity {
        identity
            .shutdown_with_timeout(Duration::from_secs(5))
            .await?;
    }
    result?;
    drained
        .map_err(|_| "NATS drain timed out")?
        .map_err(|_| "NATS drain failed")?;
    Ok(())
}

#[tokio::main]
async fn main() {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let result = match Config::load() {
        Ok(config) => run(config).await,
        Err(error) => Err(error),
    };
    if let Err(error) = result {
        eprintln!("binance-depth stopped: {error}");
        std::process::exit(1);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn image_version_cannot_be_overridden_and_unstamped_production_refuses() {
        assert_eq!(
            producer_version(Some("commit-sha"), false, Some("override".into())).unwrap(),
            "commit-sha"
        );
        assert!(producer_version(None, false, Some("override".into())).is_err());
        assert!(producer_version(Some(" "), false, None).is_err());
        assert_eq!(
            producer_version(None, true, Some("fixture".into())).unwrap(),
            "fixture"
        );
        assert!(producer_version(None, true, None).is_err());
    }
    #[test]
    fn readiness_rechecks_generation_connection_and_clock_after_ack() {
        let health = Health::default();
        health.ready.store(true, Ordering::Relaxed);
        health.connected.store(true, Ordering::Relaxed);
        health.validity.store(1, Ordering::SeqCst);
        health.book_epoch.store(1, Ordering::SeqCst);
        *health.venue_time.lock().unwrap() = Some(prost_types::Timestamp {
            seconds: 100,
            nanos: 0,
        });
        assert!(health.is_ready(prost_types::Timestamp {
            seconds: 101,
            nanos: 0
        }));
        assert!(!health.is_ready(prost_types::Timestamp {
            seconds: 106,
            nanos: 0
        }));
        assert!(!health.is_ready(prost_types::Timestamp {
            seconds: 98,
            nanos: 0
        }));
        health.validity.store(0, Ordering::SeqCst);
        assert!(!health.is_ready(prost_types::Timestamp {
            seconds: 101,
            nanos: 0
        }));
        health.validity.store(2, Ordering::SeqCst);
        assert!(!health.is_ready(prost_types::Timestamp {
            seconds: 101,
            nanos: 0
        }));
        health.book_epoch.store(2, Ordering::SeqCst);
        health.connected.store(false, Ordering::Relaxed);
        assert!(!health.is_ready(prost_types::Timestamp {
            seconds: 101,
            nanos: 0
        }));
    }
    #[test]
    fn stale_missing_and_future_venue_clocks_refuse_readiness() {
        let wall = prost_types::Timestamp {
            seconds: 100,
            nanos: 0,
        };
        assert!(!fresh(None, wall));
        assert!(fresh(
            Some(prost_types::Timestamp {
                seconds: 95,
                nanos: 0
            }),
            wall
        ));
        assert!(!fresh(
            Some(prost_types::Timestamp {
                seconds: 94,
                nanos: 999999999
            }),
            wall
        ));
        assert!(!fresh(
            Some(prost_types::Timestamp {
                seconds: 102,
                nanos: 0
            }),
            wall
        ));
    }
}

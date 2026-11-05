//! Public Binance depth transport. An unclocked REST anchor never escapes.
use crate::{
    book::Book,
    decimal,
    proto::market::v1::{OrderBookDelta, OrderBookSnapshot, PriceLevel},
};
use futures_util::{SinkExt, StreamExt};
use prost_types::Timestamp;
use serde::Deserialize;
use std::sync::{
    atomic::{AtomicU64, Ordering},
    Arc,
};
use std::{collections::VecDeque, fmt, time::Duration};

/// Independent of the bounded update queue. Zero revokes every queued book.
pub type Validity = Arc<AtomicU64>;
pub fn valid_epoch(validity: &AtomicU64, epoch: u64) -> bool {
    epoch != 0 && validity.load(Ordering::SeqCst) == epoch
}
struct InvalidateOnDrop(Validity);
impl Drop for InvalidateOnDrop {
    fn drop(&mut self) {
        self.0.store(0, Ordering::SeqCst);
    }
}
use tokio::{
    net::TcpStream,
    sync::{mpsc, watch},
    time::timeout,
};
use tokio_tungstenite::{
    connect_async_with_config,
    tungstenite::{protocol::WebSocketConfig, Message},
    MaybeTlsStream, WebSocketStream,
};

const WS_LIMIT: usize = 4 << 20;
const REST_LIMIT: usize = 8 << 20;
const QUEUE_LIMIT: usize = 8 << 20;
const FRAME_LIMIT: usize = 32;
const IO_TIMEOUT: Duration = Duration::from_secs(10);
type Socket = WebSocketStream<MaybeTlsStream<TcpStream>>;

#[derive(Clone, Debug)]
pub struct Config {
    pub instrument_id: String,
    pub symbol: String,
    pub mic: String,
    pub ws_base: String,
    pub rest_base: String,
    pub depth_limit: u16,
    pub allow_local_test: bool,
}
#[derive(Debug)]
pub enum Event {
    Reanchoring,
    Snapshot(u64, OrderBookSnapshot),
    Delta(u64, OrderBookDelta),
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SourceError {
    Configuration,
    Transport,
    Timeout,
    Malformed,
    BufferLimit,
    Cancelled,
    ReceiverClosed,
    Book,
}
impl fmt::Display for SourceError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "Binance depth source: {self:?}")
    }
}
impl std::error::Error for SourceError {}

impl Config {
    pub fn validate(&self) -> Result<(), SourceError> {
        if self.instrument_id.is_empty()
            || self.instrument_id.len() > 128
            || self.mic != "BINANCE"
            || self.symbol.is_empty()
            || self.symbol.len() > 32
            || !self
                .symbol
                .bytes()
                .all(|b| b.is_ascii_uppercase() || b.is_ascii_digit())
            || !matches!(self.depth_limit, 5 | 10 | 20 | 50 | 100 | 500 | 1000 | 5000)
        {
            return Err(SourceError::Configuration);
        }
        for (raw, production, scheme) in [
            (&self.ws_base, "stream.binance.com", "wss"),
            (&self.rest_base, "api.binance.com", "https"),
        ] {
            let url = reqwest::Url::parse(raw).map_err(|_| SourceError::Configuration)?;
            if !url.username().is_empty()
                || url.password().is_some()
                || url.query().is_some()
                || url.fragment().is_some()
                || url.path() != "/"
            {
                return Err(SourceError::Configuration);
            }
            let local = self.allow_local_test
                && matches!(url.host_str(), Some("127.0.0.1" | "localhost" | "[::1]"))
                && matches!((scheme, url.scheme()), ("wss", "ws") | ("https", "http"));
            let public = url.host_str() == Some(production)
                && url.scheme() == scheme
                && (url.port().is_none() || (scheme == "wss" && url.port() == Some(9443)));
            if !local && !public {
                return Err(SourceError::Configuration);
            }
        }
        Ok(())
    }
}
#[derive(Debug, Deserialize)]
struct Diff {
    #[serde(rename = "e")]
    event: String,
    #[serde(rename = "E")]
    time: i64,
    #[serde(rename = "s")]
    symbol: String,
    #[serde(rename = "U")]
    first: u64,
    #[serde(rename = "u")]
    last: u64,
    #[serde(rename = "b")]
    bids: Vec<Vec<String>>,
    #[serde(rename = "a")]
    asks: Vec<Vec<String>>,
}
#[derive(Deserialize)]
struct Anchor {
    #[serde(rename = "lastUpdateId")]
    last: u64,
    bids: Vec<Vec<String>>,
    asks: Vec<Vec<String>>,
}
fn stamp(ms: i64) -> Result<Timestamp, SourceError> {
    if !(1..=253_402_300_799_999).contains(&ms) {
        return Err(SourceError::Malformed);
    }
    Ok(Timestamp {
        seconds: ms / 1000,
        nanos: ((ms % 1000) * 1_000_000) as i32,
    })
}
fn parse_diff(bytes: &[u8], symbol: &str) -> Result<Diff, SourceError> {
    if bytes.len() > WS_LIMIT {
        return Err(SourceError::BufferLimit);
    }
    let d: Diff = serde_json::from_slice(bytes).map_err(|_| SourceError::Malformed)?;
    if d.event != "depthUpdate" || d.symbol != symbol || d.first == 0 || d.last < d.first {
        return Err(SourceError::Malformed);
    }
    stamp(d.time)?;
    Ok(d)
}
// Source hardening: unlike Go's per-level drop, an unsupported numeric or row
// refuses the whole frame. A bounded parser must never publish a healthy-looking
// partial book after silently dropping a level it cannot represent.
fn levels(raw: Vec<Vec<String>>) -> Result<Vec<PriceLevel>, SourceError> {
    raw.into_iter()
        .map(|v| {
            if v.len() != 2 {
                return Err(SourceError::Malformed);
            }
            let price = decimal::parse(&v[0]).ok_or(SourceError::Malformed)?;
            let size = decimal::parse(&v[1]).ok_or(SourceError::Malformed)?;
            if !kanz_decimal_compat::in_domain(Some(&price))
                || !kanz_decimal_compat::in_domain(Some(&size))
            {
                return Err(SourceError::Malformed);
            }
            Ok(PriceLevel {
                price: Some(price),
                size: Some(size),
            })
        })
        .collect()
}

fn delta(cfg: &Config, d: Diff, previous: u64) -> Result<OrderBookDelta, SourceError> {
    Ok(OrderBookDelta {
        instrument_id: cfg.instrument_id.clone(),
        symbol: cfg.symbol.clone(),
        mic: cfg.mic.clone(),
        event_time: Some(stamp(d.time)?),
        first_update_sequence: d.first,
        last_update_sequence: d.last,
        prev_update_sequence: previous,
        bids: levels(d.bids)?,
        asks: levels(d.asks)?,
    })
}
#[derive(Debug, PartialEq, Eq)]
enum Step {
    Discard,
    Apply,
    Reanchor,
}
fn sequence(last: u64, first_after_anchor: bool, d: &Diff) -> Step {
    if d.last <= last {
        return Step::Discard;
    }
    let Some(next) = last.checked_add(1) else {
        return Step::Reanchor;
    };
    if (first_after_anchor && d.first <= next) || (!first_after_anchor && d.first == next) {
        Step::Apply
    } else {
        Step::Reanchor
    }
}
fn merged(cfg: &Config, a: Anchor, d: Diff) -> Result<OrderBookSnapshot, SourceError> {
    let timestamp = stamp(d.time)?;
    let previous = a.last;
    let snap = OrderBookSnapshot {
        instrument_id: cfg.instrument_id.clone(),
        symbol: cfg.symbol.clone(),
        mic: cfg.mic.clone(),
        event_time: Some(timestamp),
        last_update_sequence: previous,
        bids: levels(a.bids)?,
        asks: levels(a.asks)?,
    };
    // Private intermediate state only. The timestamp belongs to the final
    // straddling update; no REST-only snapshot is sent or published.
    let mut book = Book::new(
        cfg.instrument_id.clone(),
        cfg.symbol.clone(),
        cfg.mic.clone(),
    );
    book.apply_snapshot(&snap).map_err(|_| SourceError::Book)?;
    book.apply_delta(&delta(cfg, d, previous)?)
        .map_err(|_| SourceError::Book)?;
    Ok(book.snapshot(0))
}
async fn rest(client: &reqwest::Client, cfg: &Config) -> Result<Anchor, SourceError> {
    let mut response = client
        .get(format!(
            "{}/api/v3/depth",
            cfg.rest_base.trim_end_matches('/')
        ))
        .query(&[
            ("symbol", cfg.symbol.as_str()),
            ("limit", &cfg.depth_limit.to_string()),
        ])
        .send()
        .await
        .map_err(|_| SourceError::Transport)?;
    if !response.status().is_success() {
        return Err(SourceError::Transport);
    }
    if response
        .content_length()
        .is_some_and(|n| n > REST_LIMIT as u64)
    {
        return Err(SourceError::BufferLimit);
    }
    let mut bytes = Vec::new();
    while let Some(chunk) = response.chunk().await.map_err(|_| SourceError::Transport)? {
        if bytes.len().saturating_add(chunk.len()) > REST_LIMIT {
            return Err(SourceError::BufferLimit);
        }
        bytes.extend_from_slice(&chunk);
    }
    let a: Anchor = serde_json::from_slice(&bytes).map_err(|_| SourceError::Malformed)?;
    if a.last == 0 {
        return Err(SourceError::Malformed);
    }
    Ok(a)
}
async fn next_frame(ws: &mut Socket) -> Result<Vec<u8>, SourceError> {
    loop {
        match ws
            .next()
            .await
            .ok_or(SourceError::Transport)?
            .map_err(|_| SourceError::Transport)?
        {
            Message::Text(v) => return Ok(v.as_bytes().to_vec()),
            Message::Binary(v) => return Ok(v.to_vec()),
            Message::Ping(v) => ws
                .send(Message::Pong(v))
                .await
                .map_err(|_| SourceError::Transport)?,
            Message::Pong(_) => {}
            Message::Close(_) => return Err(SourceError::Transport),
            _ => return Err(SourceError::Malformed),
        }
    }
}
fn enqueue(queue: &mut VecDeque<Vec<u8>>, bytes: Vec<u8>) -> Result<(), SourceError> {
    if queue.len() >= FRAME_LIMIT
        || queue
            .iter()
            .map(Vec::len)
            .sum::<usize>()
            .saturating_add(bytes.len())
            > QUEUE_LIMIT
    {
        return Err(SourceError::BufferLimit);
    }
    queue.push_back(bytes);
    Ok(())
}
async fn anchor_with_buffer(
    client: &reqwest::Client,
    cfg: &Config,
    ws: &mut Socket,
    queue: &mut VecDeque<Vec<u8>>,
) -> Result<Anchor, SourceError> {
    let fetch = rest(client, cfg);
    tokio::pin!(fetch);
    loop {
        tokio::select! {
            result=&mut fetch=>return result,
            frame=next_frame(ws)=>enqueue(queue,frame?)?,
        }
    }
}
async fn send(out: &mpsc::Sender<Event>, event: Event) -> Result<(), SourceError> {
    timeout(IO_TIMEOUT, out.send(event))
        .await
        .map_err(|_| SourceError::Timeout)?
        .map_err(|_| SourceError::ReceiverClosed)
}
async fn session(
    cfg: Config,
    out: mpsc::Sender<Event>,
    validity: Validity,
) -> Result<(), SourceError> {
    let mut epoch = 0_u64;
    cfg.validate()?;
    let client = reqwest::Client::builder()
        .no_proxy()
        .timeout(IO_TIMEOUT)
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .map_err(|_| SourceError::Configuration)?;
    let ws_config = WebSocketConfig::default()
        .max_message_size(Some(WS_LIMIT))
        .max_frame_size(Some(WS_LIMIT));
    let url = format!(
        "{}/ws/{}@depth@100ms",
        cfg.ws_base.trim_end_matches('/'),
        cfg.symbol.to_ascii_lowercase()
    );
    let (mut ws, _) = timeout(
        IO_TIMEOUT,
        connect_async_with_config(url, Some(ws_config), false),
    )
    .await
    .map_err(|_| SourceError::Timeout)?
    .map_err(|_| SourceError::Transport)?;
    let mut queue = VecDeque::new();
    let mut anchor = Some(
        timeout(
            IO_TIMEOUT,
            anchor_with_buffer(&client, &cfg, &mut ws, &mut queue),
        )
        .await
        .map_err(|_| SourceError::Timeout)??,
    );
    let mut last = anchor.as_ref().ok_or(SourceError::Malformed)?.last;
    loop {
        let bytes = match queue.pop_front() {
            Some(v) => v,
            None => timeout(IO_TIMEOUT, next_frame(&mut ws))
                .await
                .map_err(|_| SourceError::Timeout)??,
        };
        let d = parse_diff(&bytes, &cfg.symbol)?;
        match sequence(last, anchor.is_some(), &d) {
            Step::Discard => continue,
            Step::Reanchor => {
                validity.store(0, Ordering::SeqCst);
                send(&out, Event::Reanchoring).await?;
                // Preserve the gapped frame: the refreshed REST anchor may be
                // old enough for this exact frame to become its straddler.
                queue.push_front(bytes);
                let fresh = timeout(
                    IO_TIMEOUT,
                    anchor_with_buffer(&client, &cfg, &mut ws, &mut queue),
                )
                .await
                .map_err(|_| SourceError::Timeout)??;
                if fresh.last <= last {
                    return Err(SourceError::Malformed);
                }
                last = fresh.last;
                anchor = Some(fresh);
            }
            Step::Apply => {
                last = d.last;
                if let Some(a) = anchor.take() {
                    let snapshot = merged(&cfg, a, d)?;
                    epoch = epoch.checked_add(1).ok_or(SourceError::Malformed)?;
                    validity.store(epoch, Ordering::SeqCst);
                    send(&out, Event::Snapshot(epoch, snapshot)).await?;
                } else {
                    let previous = d.first - 1;
                    send(&out, Event::Delta(epoch, delta(&cfg, d, previous)?)).await?;
                }
            }
        }
    }
}
/// Any disconnect/overflow/timeout fails the source and must clear readiness.
/// Restarting run opens a new stream before fetching a fresh REST anchor; it
/// never resumes from an old sequence after a disconnected session.
pub async fn run(
    cfg: Config,
    out: mpsc::Sender<Event>,
    mut cancel: watch::Receiver<bool>,
    validity: Validity,
) -> Result<(), SourceError> {
    validity.store(0, Ordering::SeqCst);
    let _invalidate = InvalidateOnDrop(validity.clone());
    if *cancel.borrow() {
        return Err(SourceError::Cancelled);
    }
    tokio::select! {
        result=session(cfg,out,validity)=>result,
        _=async {loop {if cancel.changed().await.is_err() || *cancel.borrow() {break;}}}=>Err(SourceError::Cancelled),
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn source_scope_exit_revokes_without_draining_queue() {
        let validity = Arc::new(AtomicU64::new(1));
        {
            let _guard = InvalidateOnDrop(validity.clone());
            assert!(valid_epoch(&validity, 1));
        }
        assert!(!valid_epoch(&validity, 1));
        assert_eq!(validity.load(Ordering::SeqCst), 0);
    }
    fn cfg() -> Config {
        Config {
            instrument_id: "BTC-USD".into(),
            symbol: "BTCUSDT".into(),
            mic: "BINANCE".into(),
            ws_base: "wss://stream.binance.com:9443".into(),
            rest_base: "https://api.binance.com".into(),
            depth_limit: 1000,
            allow_local_test: false,
        }
    }
    fn d(first: u64, last: u64) -> Diff {
        Diff {
            event: "depthUpdate".into(),
            time: 2000,
            symbol: "BTCUSDT".into(),
            first,
            last,
            bids: vec![],
            asks: vec![],
        }
    }
    #[test]
    fn anchor_straddle_chain_duplicate_and_gap() {
        assert_eq!(sequence(100, true, &d(90, 95)), Step::Discard);
        assert_eq!(sequence(100, true, &d(99, 101)), Step::Apply);
        assert_eq!(sequence(101, false, &d(102, 103)), Step::Apply);
        assert_eq!(sequence(103, false, &d(102, 103)), Step::Discard);
        assert_eq!(sequence(101, false, &d(150, 151)), Step::Reanchor);
        assert_eq!(sequence(100, true, &d(102, 103)), Step::Reanchor);
    }
    #[test]
    fn merged_anchor_uses_first_venue_time_and_deletes_zero() {
        let a = Anchor {
            last: 100,
            bids: vec![vec!["50000".into(), "1".into()]],
            asks: vec![vec!["50010".into(), "2".into()]],
        };
        let mut first = d(99, 101);
        first.bids = vec![
            vec!["50000".into(), "0".into()],
            vec!["50001".into(), "3".into()],
        ];
        let result = merged(&cfg(), a, first).unwrap();
        assert_eq!(result.last_update_sequence, 101);
        assert_eq!(result.event_time.unwrap().seconds, 2);
        assert_eq!(result.bids.len(), 1);
    }
    #[test]
    fn no_timestamp_or_wrong_symbol_is_refused() {
        for bytes in [
            br#"{"e":"depthUpdate","E":0,"s":"BTCUSDT","U":1,"u":1,"b":[],"a":[]}"#.as_slice(),
            br#"{"e":"depthUpdate","E":1,"s":"ETHUSDT","U":1,"u":1,"b":[],"a":[]}"#.as_slice(),
        ] {
            assert!(parse_diff(bytes, "BTCUSDT").is_err());
        }
    }
    #[test]
    fn endpoints_require_production_or_explicit_local_opt_in() {
        let mut c = cfg();
        assert!(c.validate().is_ok());
        c.ws_base = "ws://127.0.0.1:1234".into();
        assert!(c.validate().is_err());
        c.allow_local_test = true;
        assert!(c.validate().is_ok());
        c.ws_base = "wss://stream.binance.com.evil.test".into();
        assert!(c.validate().is_err());
    }
    #[test]
    fn buffers_and_zero_size_are_bounded() {
        let mut queue = VecDeque::new();
        for _ in 0..FRAME_LIMIT {
            enqueue(&mut queue, vec![1]).unwrap();
        }
        assert_eq!(enqueue(&mut queue, vec![1]), Err(SourceError::BufferLimit));
        let parsed = levels(vec![vec!["1".into(), "0".into()]]).unwrap();
        assert!(levels(vec![vec!["1e100".into(), "1".into()]]).is_err());
        let oversized = format!("{}e64", "9".repeat(125));
        assert!(decimal::parse(&oversized).is_some());
        assert!(levels(vec![vec![oversized.clone(), "1".into()]]).is_err());
        assert!(levels(vec![vec!["1".into(), oversized]]).is_err());
        assert!(levels(vec![vec!["bad".into(), "1".into()]]).is_err());
        assert!(levels(vec![vec!["1".into()]]).is_err());
        assert_eq!(parsed.len(), 1);
        assert_eq!(parsed[0].size.as_ref().unwrap().coefficient, 0);
    }
    #[tokio::test]
    async fn cancellation_before_connect_is_explicit() {
        let (tx, _rx) = mpsc::channel(1);
        let (_cancel, rx) = watch::channel(true);
        assert_eq!(
            run(cfg(), tx, rx, Arc::new(AtomicU64::new(0))).await,
            Err(SourceError::Cancelled)
        );
    }
    #[tokio::test]
    async fn local_protocol_opens_stream_then_emits_only_merged_anchor() {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        let ws_listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let http_listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let mut config = cfg();
        config.allow_local_test = true;
        config.ws_base = format!("ws://{}", ws_listener.local_addr().unwrap());
        config.rest_base = format!("http://{}", http_listener.local_addr().unwrap());
        let (opened_tx, opened_rx) = tokio::sync::oneshot::channel();
        let (release_tx, release_rx) = tokio::sync::oneshot::channel();
        let (rest_done_tx, rest_done_rx) = tokio::sync::oneshot::channel();
        let socket_task = tokio::spawn(async move {
            let (stream, _) = ws_listener.accept().await.unwrap();
            let mut ws = tokio_tungstenite::accept_async(stream).await.unwrap();
            opened_tx.send(()).unwrap();
            release_rx.await.unwrap();
            for body in [
                r#"{"e":"depthUpdate","E":1000,"s":"BTCUSDT","U":90,"u":95,"b":[],"a":[]}"#,
                r#"{"e":"depthUpdate","E":2000,"s":"BTCUSDT","U":99,"u":101,"b":[["50000","0"],["50001","3"]],"a":[]}"#,
                r#"{"e":"depthUpdate","E":3000,"s":"BTCUSDT","U":102,"u":103,"b":[],"a":[]}"#,
                r#"{"e":"depthUpdate","E":4000,"s":"BTCUSDT","U":150,"u":151,"b":[],"a":[]}"#,
                r#"{"e":"depthUpdate","E":5000,"s":"BTCUSDT","U":201,"u":201,"b":[],"a":[]}"#,
            ] {
                ws.send(Message::Text(body.into())).await.unwrap();
            }
            std::future::pending::<()>().await;
        });
        let rest_task = tokio::spawn(async move {
            opened_rx.await.unwrap();
            let mut done = Some(rest_done_tx);
            for sequence in [100, 200] {
                let (mut stream, _) = http_listener.accept().await.unwrap();
                let mut request = [0; 2048];
                stream.read(&mut request).await.unwrap();
                let body = format!(
                    r#"{{"lastUpdateId":{sequence},"bids":[["50000","1"]],"asks":[["50010","2"]]}}"#
                );
                let response = format!(
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                    body.len(),
                    body
                );
                stream.write_all(response.as_bytes()).await.unwrap();
                if let Some(done) = done.take() {
                    done.send(()).unwrap();
                }
            }
        });
        let (tx, mut rx) = mpsc::channel(2);
        let (stop, cancel) = watch::channel(false);
        let source = tokio::spawn(run(config, tx, cancel, Arc::new(AtomicU64::new(0))));
        timeout(Duration::from_secs(3), rest_done_rx)
            .await
            .unwrap()
            .unwrap();
        assert!(
            timeout(Duration::from_millis(50), rx.recv()).await.is_err(),
            "REST-only anchor escaped without venue time"
        );
        release_tx.send(()).unwrap();
        let first = timeout(Duration::from_secs(3), rx.recv())
            .await
            .unwrap()
            .unwrap();
        let Event::Snapshot(_, snapshot) = first else {
            panic!("first emission must be merged snapshot");
        };
        assert_eq!(snapshot.last_update_sequence, 101);
        assert_eq!(snapshot.event_time.unwrap().seconds, 2);
        assert_eq!(snapshot.bids.len(), 1);
        let second = timeout(Duration::from_secs(3), rx.recv())
            .await
            .unwrap()
            .unwrap();
        let Event::Delta(_, delta) = second else {
            panic!("expected chained delta");
        };
        assert_eq!(delta.prev_update_sequence, 101);
        assert_eq!(delta.last_update_sequence, 103);
        assert!(matches!(
            timeout(Duration::from_secs(3), rx.recv())
                .await
                .unwrap()
                .unwrap(),
            Event::Reanchoring
        ));
        let Event::Snapshot(_, reanchored) = timeout(Duration::from_secs(3), rx.recv())
            .await
            .unwrap()
            .unwrap()
        else {
            panic!("gap must emit fresh merged anchor");
        };
        assert_eq!(reanchored.last_update_sequence, 201);
        assert_eq!(reanchored.event_time.unwrap().seconds, 5);
        stop.send(true).unwrap();
        assert_eq!(
            timeout(Duration::from_secs(3), source)
                .await
                .unwrap()
                .unwrap(),
            Err(SourceError::Cancelled)
        );
        rest_task.await.unwrap();
        socket_task.abort();
        let _ = socket_task.await;
    }
}

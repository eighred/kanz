//! Venue sequencing is explicit, including maintenance resets to zero.
use futures_util::{SinkExt, StreamExt};
use kanz_market_feed::{
    decimal,
    proto::market::v1::{OrderBookDelta, OrderBookSnapshot, PriceLevel},
    source::{Event, InvalidateOnDrop, Validity},
};
use prost_types::Timestamp;
use serde::Deserialize;
use std::{fmt, sync::atomic::Ordering, time::Duration};
use tokio::{
    net::TcpStream,
    sync::{mpsc, watch},
    time::{timeout, Instant},
};
use tokio_tungstenite::{
    connect_async_with_config,
    tungstenite::{protocol::WebSocketConfig, Message},
    MaybeTlsStream, WebSocketStream,
};

const LIMIT: usize = 4 << 20;
const MAX_ROWS: usize = 1000;
const MAX_UPDATES: usize = 32;
const IO_TIMEOUT: Duration = Duration::from_secs(10);
const PING_AFTER: Duration = Duration::from_secs(15);
const PONG_WAIT: Duration = Duration::from_secs(5);
type Socket = WebSocketStream<MaybeTlsStream<TcpStream>>;

#[derive(Clone, Debug)]
pub struct Config {
    pub instrument_id: String,
    pub symbol: String,
    pub ws_url: String,
    pub allow_local_test: bool,
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SourceError {
    Configuration,
    Transport,
    Timeout,
    Malformed,
    Subscription,
    Gap,
    Cancelled,
    ReceiverClosed,
}
impl fmt::Display for SourceError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "OKX depth source: {self:?}")
    }
}
impl std::error::Error for SourceError {}

impl Config {
    pub fn validate(&self) -> Result<(), SourceError> {
        if self.instrument_id.trim().is_empty()
            || self.instrument_id.len() > 128
            || self.symbol.is_empty()
            || self.symbol.len() > 64
            || !self
                .symbol
                .bytes()
                .all(|b| b.is_ascii_uppercase() || b.is_ascii_digit() || b == b'-')
        {
            return Err(SourceError::Configuration);
        }
        let u = url::Url::parse(&self.ws_url).map_err(|_| SourceError::Configuration)?;
        if !u.username().is_empty()
            || u.password().is_some()
            || u.query().is_some()
            || u.fragment().is_some()
            || u.path() != "/ws/v5/public"
        {
            return Err(SourceError::Configuration);
        }
        let public =
            u.scheme() == "wss" && u.host_str() == Some("ws.okx.com") && u.port() == Some(8443);
        let local = self.allow_local_test
            && u.scheme() == "ws"
            && matches!(u.host_str(), Some("127.0.0.1" | "[::1]"));
        if !public && !local {
            return Err(SourceError::Configuration);
        }
        Ok(())
    }
}

#[derive(Deserialize)]
struct Arg {
    channel: String,
    #[serde(rename = "instId")]
    instrument: String,
}
#[derive(Deserialize)]
struct Wire {
    #[serde(default)]
    event: Option<String>,
    #[serde(default)]
    action: Option<String>,
    #[serde(default)]
    arg: Option<Arg>,
    #[serde(default)]
    data: Option<Vec<Rows>>,
}
#[derive(Deserialize)]
struct Rows {
    bids: Vec<Vec<String>>,
    asks: Vec<Vec<String>>,
    ts: String,
    #[serde(rename = "seqId")]
    sequence: i64,
    #[serde(rename = "prevSeqId")]
    previous: i64,
}
#[derive(Debug)]
struct Update {
    snapshot: bool,
    sequence: u64,
    previous: i64,
    time: Timestamp,
    bids: Vec<PriceLevel>,
    asks: Vec<PriceLevel>,
}
#[derive(Debug)]
enum Frame {
    Ack,
    Pong,
    Updates(Vec<Update>),
}

fn levels(rows: Vec<Vec<String>>) -> Result<Vec<PriceLevel>, SourceError> {
    if rows.len() > MAX_ROWS {
        return Err(SourceError::Malformed);
    }
    rows.into_iter()
        .map(|r| {
            if r.len() != 4 {
                return Err(SourceError::Malformed);
            }
            let p = decimal::parse(&r[0]).ok_or(SourceError::Malformed)?;
            let s = decimal::parse(&r[1]).ok_or(SourceError::Malformed)?;
            if !kanz_decimal_compat::in_domain(Some(&p))
                || !kanz_decimal_compat::in_domain(Some(&s))
                || p.coefficient <= 0
                || s.coefficient < 0
            {
                return Err(SourceError::Malformed);
            }
            // The final two fields are deprecated liquidity count and order count;
            // retain neither, but refuse malformed tuple structure atomically.
            if r[2] != "0"
                || r[3].is_empty()
                || r[3].len() > 20
                || !r[3].bytes().all(|b| b.is_ascii_digit())
            {
                return Err(SourceError::Malformed);
            }
            Ok(PriceLevel {
                price: Some(p),
                size: Some(s),
            })
        })
        .collect()
}
fn decode(bytes: &[u8], symbol: &str) -> Result<Frame, SourceError> {
    if bytes.len() > LIMIT {
        return Err(SourceError::Malformed);
    }
    if bytes == b"pong" {
        return Ok(Frame::Pong);
    }
    let w: Wire = serde_json::from_slice(bytes).map_err(|_| SourceError::Malformed)?;
    if w.event.as_deref() == Some("error") {
        return Err(SourceError::Subscription);
    }
    let arg = w.arg.ok_or(SourceError::Malformed)?;
    if arg.channel != "books" || arg.instrument != symbol {
        return Err(SourceError::Malformed);
    }
    if let Some(event) = w.event {
        return if event == "subscribe" && w.action.is_none() && w.data.is_none() {
            Ok(Frame::Ack)
        } else {
            Err(SourceError::Malformed)
        };
    }
    let snapshot = match w.action.as_deref() {
        Some("snapshot") => true,
        Some("update") => false,
        _ => return Err(SourceError::Malformed),
    };
    let rows = w.data.ok_or(SourceError::Malformed)?;
    if rows.is_empty() || rows.len() > MAX_UPDATES {
        return Err(SourceError::Malformed);
    }
    let mut updates = Vec::with_capacity(rows.len());
    for r in rows {
        if r.sequence < 0 || (snapshot && r.previous != -1) || (!snapshot && r.previous < 0) {
            return Err(SourceError::Malformed);
        }
        let ms = r.ts.parse::<i64>().map_err(|_| SourceError::Malformed)?;
        if !(1..=253_402_300_799_999).contains(&ms) {
            return Err(SourceError::Malformed);
        }
        updates.push(Update {
            snapshot,
            sequence: r.sequence as u64,
            previous: r.previous,
            time: Timestamp {
                seconds: ms / 1000,
                nanos: ((ms % 1000) * 1_000_000) as i32,
            },
            bids: levels(r.bids)?,
            asks: levels(r.asks)?,
        });
    }
    Ok(Frame::Updates(updates))
}

// Check the entire decoded frame before emitting any constituent update. A
// repeated sequence is a keepalive only when both sides are empty and chained.
fn advance(last: Option<u64>, updates: &[Update]) -> Result<Option<u64>, SourceError> {
    let mut next = last;
    for u in updates {
        if u.snapshot {
            next = Some(u.sequence);
            continue;
        }
        let previous = next.ok_or(SourceError::Gap)?;
        if u.previous as u64 != previous {
            return Err(SourceError::Gap);
        }
        if u.sequence == previous && (!u.bids.is_empty() || !u.asks.is_empty()) {
            return Err(SourceError::Malformed);
        }
        next = Some(u.sequence);
    }
    Ok(next)
}
async fn send(out: &mpsc::Sender<Event>, event: Event) -> Result<(), SourceError> {
    timeout(IO_TIMEOUT, out.send(event))
        .await
        .map_err(|_| SourceError::Timeout)?
        .map_err(|_| SourceError::ReceiverClosed)
}

async fn connected(
    cfg: &Config,
    out: &mpsc::Sender<Event>,
    validity: &Validity,
    epoch: &mut u64,
) -> Result<(), SourceError> {
    let ws_cfg = WebSocketConfig::default()
        .max_message_size(Some(LIMIT))
        .max_frame_size(Some(LIMIT));
    let (mut ws, _) = timeout(
        IO_TIMEOUT,
        connect_async_with_config(&cfg.ws_url, Some(ws_cfg), false),
    )
    .await
    .map_err(|_| SourceError::Timeout)?
    .map_err(|_| SourceError::Transport)?;
    let sub =
        serde_json::json!({"op":"subscribe","args":[{"channel":"books","instId":cfg.symbol}]})
            .to_string();
    write(&mut ws, Message::Text(sub.into())).await?;
    let mut acknowledged = false;
    let mut last = None;
    let mut received = Instant::now();
    let anchor_deadline = Instant::now() + IO_TIMEOUT;
    let mut pong_deadline = None;
    loop {
        let deadline = if !acknowledged || last.is_none() {
            anchor_deadline
        } else {
            pong_deadline.unwrap_or(received + PING_AFTER)
        };
        let message = match tokio::time::timeout_at(deadline, ws.next()).await {
            Ok(Some(Ok(m))) => m,
            Ok(_) => return Err(SourceError::Transport),
            Err(_) => {
                if !acknowledged || last.is_none() || pong_deadline.is_some() {
                    return Err(SourceError::Timeout);
                }
                write(&mut ws, Message::Text("ping".into())).await?;
                pong_deadline = Some(Instant::now() + PONG_WAIT);
                continue;
            }
        };
        let frame = match message {
            Message::Text(text) => decode(text.as_bytes(), &cfg.symbol)?,
            Message::Ping(bytes) => {
                write(&mut ws, Message::Pong(bytes)).await?;
                continue;
            }
            Message::Pong(_) => continue,
            _ => return Err(SourceError::Malformed),
        };
        received = Instant::now();
        match frame {
            Frame::Ack => {
                if acknowledged {
                    return Err(SourceError::Malformed);
                }
                acknowledged = true
            }
            Frame::Pong => {
                if pong_deadline.is_none() {
                    return Err(SourceError::Malformed);
                }
                pong_deadline = None
            }
            Frame::Updates(updates) => {
                if !acknowledged {
                    return Err(SourceError::Subscription);
                }
                let next = advance(last, &updates)?;
                for u in updates {
                    if u.snapshot {
                        // A replacement anchor revokes queued prior epochs before
                        // waiting on the bounded publication queue.
                        validity.store(0, Ordering::SeqCst);
                        *epoch = epoch.checked_add(1).ok_or(SourceError::Malformed)?;
                        validity.store(*epoch, Ordering::SeqCst);
                        let snapshot = OrderBookSnapshot {
                            instrument_id: cfg.instrument_id.clone(),
                            symbol: cfg.symbol.clone(),
                            mic: "OKX".into(),
                            event_time: Some(u.time),
                            last_update_sequence: u.sequence,
                            bids: u.bids,
                            asks: u.asks,
                        };
                        send(out, Event::Snapshot(*epoch, snapshot)).await?;
                    } else if Some(u.sequence) != last {
                        let delta = OrderBookDelta {
                            instrument_id: cfg.instrument_id.clone(),
                            symbol: cfg.symbol.clone(),
                            mic: "OKX".into(),
                            event_time: Some(u.time),
                            first_update_sequence: u.sequence,
                            last_update_sequence: u.sequence,
                            prev_update_sequence: u.previous as u64,
                            bids: u.bids,
                            asks: u.asks,
                        };
                        send(out, Event::Delta(*epoch, delta)).await?;
                    }
                    last = Some(u.sequence);
                }
                last = next;
            }
        }
    }
}
async fn write(ws: &mut Socket, message: Message) -> Result<(), SourceError> {
    timeout(IO_TIMEOUT, ws.send(message))
        .await
        .map_err(|_| SourceError::Timeout)?
        .map_err(|_| SourceError::Transport)
}

async fn session(
    cfg: Config,
    out: mpsc::Sender<Event>,
    validity: Validity,
) -> Result<(), SourceError> {
    cfg.validate()?;
    if rustls::crypto::CryptoProvider::get_default().is_none() {
        let _ = rustls::crypto::ring::default_provider().install_default();
    }
    let mut epoch = 0;
    loop {
        match connected(&cfg, &out, &validity, &mut epoch).await {
            Err(SourceError::Gap) => {
                validity.store(0, Ordering::SeqCst);
                send(&out, Event::Reanchoring).await?;
                // Avoid a hot reconnect loop against a persistently bad feed.
                tokio::time::sleep(Duration::from_millis(250)).await;
            }
            result => return result,
        }
    }
}
/// Malformed input, transport loss and deadlines terminate the source. A gap
/// reconnects for a new subscription snapshot; neither path retains validity.
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
        _=async{loop{if cancel.changed().await.is_err()||*cancel.borrow(){break}}}=>Err(SourceError::Cancelled),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use kanz_market_feed::{book::Book, source::valid_epoch};
    use std::sync::{atomic::AtomicU64, Arc};
    use tokio::net::TcpListener;
    use tokio_tungstenite::accept_async;

    fn config() -> Config {
        Config {
            instrument_id: "BTC-USD".into(),
            symbol: "BTC-USDT".into(),
            ws_url: "wss://ws.okx.com:8443/ws/v5/public".into(),
            allow_local_test: false,
        }
    }
    fn ack() -> String {
        r#"{"event":"subscribe","arg":{"channel":"books","instId":"BTC-USDT"}}"#.into()
    }
    fn book(action: &str, seq: i64, prev: i64) -> serde_json::Value {
        serde_json::json!({"arg":{"channel":"books","instId":"BTC-USDT"},"action":action,"data":[{"bids":[["100","2","0","1"]],"asks":[["101","3","0","1"]],"ts":"1700000000000","seqId":seq,"prevSeqId":prev,"checksum":0}]})
    }
    fn updates(v: serde_json::Value) -> Vec<Update> {
        match decode(v.to_string().as_bytes(), "BTC-USDT").unwrap() {
            Frame::Updates(u) => u,
            _ => panic!("expected data"),
        }
    }

    #[test]
    fn production_endpoint_and_development_are_explicit() {
        assert!(config().validate().is_ok());
        for endpoint in [
            "wss://ws.okx.com.evil:8443/ws/v5/public",
            "wss://ws.okx.com:8443/ws/v5/private",
            "wss://user@ws.okx.com:8443/ws/v5/public",
            "wss://ws.okx.com:8443/ws/v5/public?q=1",
            "ws://127.0.0.1:1234/ws/v5/public",
        ] {
            let mut c = config();
            c.ws_url = endpoint.into();
            assert_eq!(c.validate(), Err(SourceError::Configuration));
        }
        let mut c = config();
        c.allow_local_test = true;
        c.ws_url = "ws://127.0.0.1:1234/ws/v5/public".into();
        assert!(c.validate().is_ok());
        c.ws_url = "ws://192.0.2.1:1234/ws/v5/public".into();
        assert!(c.validate().is_err());
    }
    #[test]
    fn complete_frame_identity_action_time_and_rows_are_required() {
        let cases = [
            ("instrument", 0),
            ("channel", 1),
            ("action", 2),
            ("time", 3),
            ("number", 4),
            ("tuple", 5),
            ("negative_sequence", 6),
            ("snapshot_previous", 7),
            ("negative_size", 8),
            ("zero_price", 9),
        ];
        for (name, kind) in cases {
            let mut v = book("snapshot", 100, -1);
            match kind {
                0 => v["arg"]["instId"] = "OTHER".into(),
                1 => v["arg"]["channel"] = "trades".into(),
                2 => v["action"] = "mystery".into(),
                3 => v["data"][0]["ts"] = "0".into(),
                4 => v["data"][0]["asks"][0][0] = "123456789123456789e64".into(),
                5 => v["data"][0]["bids"][0] = serde_json::json!(["100"]),
                6 => v["data"][0]["seqId"] = (-2).into(),
                7 => v["data"][0]["prevSeqId"] = 0.into(),
                8 => v["data"][0]["bids"][0][1] = "-1".into(),
                _ => v["data"][0]["asks"][0][0] = "0".into(),
            };
            assert!(
                decode(v.to_string().as_bytes(), "BTC-USDT").is_err(),
                "{name}"
            );
        }
    }
    #[test]
    fn malformed_later_row_refuses_entire_frame() {
        let mut v = book("snapshot", 1, -1);
        let mut invalid = v["data"][0].clone();
        invalid["ts"] = "bad".into();
        v["data"].as_array_mut().unwrap().push(invalid);
        assert!(decode(v.to_string().as_bytes(), "BTC-USDT").is_err());
    }
    #[test]
    fn frame_and_row_bounds_are_enforced() {
        assert!(decode(&vec![b'x'; LIMIT + 1], "BTC-USDT").is_err());
        let mut v = book("snapshot", 1, -1);
        v["data"][0]["bids"] = serde_json::json!(vec![vec!["100", "1", "0", "1"]; MAX_ROWS + 1]);
        assert!(decode(v.to_string().as_bytes(), "BTC-USDT").is_err());
    }
    #[test]
    fn reset_to_zero_chains_without_unsigned_wrapping() {
        let s = updates(book("snapshot", 100, -1));
        assert_eq!(advance(None, &s), Ok(Some(100)));
        let reset = updates(book("update", 0, 100));
        assert_eq!(advance(Some(100), &reset), Ok(Some(0)));
        assert_eq!(
            advance(Some(0), &updates(book("update", 1, 0))),
            Ok(Some(1))
        );
        assert_eq!(
            advance(Some(0), &updates(book("update", 1, 100))),
            Err(SourceError::Gap)
        );
    }
    #[test]
    fn keepalive_requires_empty_rows_and_exact_previous() {
        assert_eq!(
            advance(Some(100), &updates(book("update", 100, 100))),
            Err(SourceError::Malformed)
        );
        let mut v = book("update", 100, 100);
        v["data"][0]["bids"] = serde_json::json!([]);
        v["data"][0]["asks"] = serde_json::json!([]);
        assert_eq!(advance(Some(100), &updates(v.clone())), Ok(Some(100)));
        v["data"][0]["prevSeqId"] = 99.into();
        assert_eq!(advance(Some(100), &updates(v)), Err(SourceError::Gap));
    }
    #[test]
    fn controls_do_not_masquerade_as_books() {
        assert!(matches!(
            decode(ack().as_bytes(), "BTC-USDT"),
            Ok(Frame::Ack)
        ));
        assert_eq!(
            decode(br#"{"event":"error","code":"60012"}"#, "BTC-USDT").unwrap_err(),
            SourceError::Subscription
        );
        assert!(decode(
            br#"{"event":"unsubscribe","arg":{"channel":"books","instId":"BTC-USDT"}}"#,
            "BTC-USDT"
        )
        .is_err());
    }

    async fn listener() -> (TcpListener, Config) {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let mut cfg = config();
        cfg.allow_local_test = true;
        cfg.ws_url = format!("ws://{}/ws/v5/public", listener.local_addr().unwrap());
        (listener, cfg)
    }
    async fn receive(rx: &mut mpsc::Receiver<Event>) -> Event {
        timeout(Duration::from_secs(3), rx.recv())
            .await
            .unwrap()
            .unwrap()
    }

    #[tokio::test]
    async fn protocol_snapshot_reset_zero_and_delta_use_shared_book() {
        let (listener, cfg) = listener().await;
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut ws = accept_async(stream).await.unwrap();
            let sub = ws.next().await.unwrap().unwrap();
            let value: serde_json::Value = serde_json::from_str(sub.to_text().unwrap()).unwrap();
            assert_eq!(value["args"][0]["channel"], "books");
            assert_eq!(value["args"][0]["instId"], "BTC-USDT");
            ws.send(Message::Text(ack().into())).await.unwrap();
            for v in [
                book("snapshot", 100, -1),
                book("update", 0, 100),
                book("update", 1, 0),
            ] {
                ws.send(Message::Text(v.to_string().into())).await.unwrap()
            }
            let _ = ws.next().await;
        });
        let (tx, mut rx) = mpsc::channel(4);
        let (cancel, cancel_rx) = watch::channel(false);
        let validity = Arc::new(AtomicU64::new(0));
        let worker = tokio::spawn(run(cfg, tx, cancel_rx, validity.clone()));
        let mut folded = Book::new("BTC-USD".into(), "BTC-USDT".into(), "OKX".into());
        match receive(&mut rx).await {
            Event::Snapshot(epoch, s) => {
                assert!(valid_epoch(&validity, epoch));
                folded.apply_snapshot(&s).unwrap()
            }
            _ => panic!("snapshot required"),
        }
        for seq in [0, 1] {
            match receive(&mut rx).await {
                Event::Delta(_, d) => {
                    assert_eq!(d.last_update_sequence, seq);
                    folded.apply_delta_observed(&d).unwrap();
                    assert_eq!(folded.snapshot(0).last_update_sequence, seq)
                }
                _ => panic!("observed delta required"),
            }
        }
        cancel.send(true).unwrap();
        assert_eq!(worker.await.unwrap(), Err(SourceError::Cancelled));
        assert_eq!(validity.load(Ordering::SeqCst), 0);
        server.abort();
    }
    #[tokio::test]
    async fn gap_revokes_even_when_reanchor_queue_is_full() {
        let (listener, cfg) = listener().await;
        let (trigger, mut trigger_rx) = watch::channel(false);
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut ws = accept_async(stream).await.unwrap();
            let _ = ws.next().await;
            ws.send(Message::Text(ack().into())).await.unwrap();
            ws.send(Message::Text(book("snapshot", 100, -1).to_string().into()))
                .await
                .unwrap();
            trigger_rx.changed().await.unwrap();
            for v in [book("update", 101, 100), book("update", 999, 998)] {
                ws.send(Message::Text(v.to_string().into())).await.unwrap()
            }
            let _ = ws.next().await;
        });
        let (tx, mut rx) = mpsc::channel(1);
        let (cancel, cancel_rx) = watch::channel(false);
        let validity = Arc::new(AtomicU64::new(0));
        let worker = tokio::spawn(run(cfg, tx, cancel_rx, validity.clone()));
        assert!(matches!(receive(&mut rx).await, Event::Snapshot(_, _)));
        assert_ne!(validity.load(Ordering::SeqCst), 0);
        trigger.send(true).unwrap();
        timeout(Duration::from_secs(3), async {
            while validity.load(Ordering::SeqCst) != 0 {
                tokio::task::yield_now().await
            }
        })
        .await
        .unwrap();
        assert_eq!(rx.len(), 1); // Delta fills queue: Reanchoring cannot yet be sent.
        cancel.send(true).unwrap();
        assert_eq!(worker.await.unwrap(), Err(SourceError::Cancelled));
        server.abort();
    }
    #[tokio::test]
    async fn snapshot_before_ack_fails_without_publication() {
        let (listener, cfg) = listener().await;
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut ws = accept_async(stream).await.unwrap();
            let _ = ws.next().await;
            ws.send(Message::Text(book("snapshot", 1, -1).to_string().into()))
                .await
                .unwrap();
            let _ = ws.next().await;
        });
        let (tx, mut rx) = mpsc::channel(1);
        let (_cancel, cancel_rx) = watch::channel(false);
        let validity = Arc::new(AtomicU64::new(0));
        assert_eq!(
            run(cfg, tx, cancel_rx, validity.clone()).await,
            Err(SourceError::Subscription)
        );
        assert!(rx.try_recv().is_err());
        assert_eq!(validity.load(Ordering::SeqCst), 0);
        server.abort();
    }

    #[tokio::test]
    async fn gap_reconnect_requires_new_ack_and_snapshot_epoch() {
        let (listener, cfg) = listener().await;
        let (trigger, mut trigger_rx) = watch::channel(false);
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut ws = accept_async(stream).await.unwrap();
            let _ = ws.next().await;
            ws.send(Message::Text(ack().into())).await.unwrap();
            ws.send(Message::Text(book("snapshot", 100, -1).to_string().into()))
                .await
                .unwrap();
            trigger_rx.changed().await.unwrap();
            ws.send(Message::Text(book("update", 500, 499).to_string().into()))
                .await
                .unwrap();
            let (stream, _) = listener.accept().await.unwrap();
            let mut replacement = accept_async(stream).await.unwrap();
            let _ = replacement.next().await;
            replacement.send(Message::Text(ack().into())).await.unwrap();
            replacement
                .send(Message::Text(book("snapshot", 0, -1).to_string().into()))
                .await
                .unwrap();
            let _ = replacement.next().await;
        });
        let (tx, mut rx) = mpsc::channel(4);
        let (cancel, cancel_rx) = watch::channel(false);
        let validity = Arc::new(AtomicU64::new(0));
        let worker = tokio::spawn(run(cfg, tx, cancel_rx, validity.clone()));
        let first = match receive(&mut rx).await {
            Event::Snapshot(e, _) => e,
            _ => panic!("initial snapshot"),
        };
        trigger.send(true).unwrap();
        assert!(matches!(receive(&mut rx).await, Event::Reanchoring));
        match receive(&mut rx).await {
            Event::Snapshot(e, s) => {
                assert!(e > first);
                assert_eq!(s.last_update_sequence, 0);
                assert!(valid_epoch(&validity, e));
                assert!(!valid_epoch(&validity, first))
            }
            _ => panic!("replacement snapshot"),
        }
        cancel.send(true).unwrap();
        assert_eq!(worker.await.unwrap(), Err(SourceError::Cancelled));
        server.abort();
    }

    #[tokio::test]
    async fn application_ping_pong_never_refreshes_book_time() {
        let (listener, cfg) = listener().await;
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut ws = accept_async(stream).await.unwrap();
            let _ = ws.next().await;
            ws.send(Message::Text(ack().into())).await.unwrap();
            ws.send(Message::Text(book("snapshot", 1, -1).to_string().into()))
                .await
                .unwrap();
            let ping = timeout(PING_AFTER + Duration::from_secs(3), ws.next())
                .await
                .unwrap()
                .unwrap()
                .unwrap();
            assert_eq!(ping.to_text().unwrap(), "ping");
            ws.send(Message::Text("pong".into())).await.unwrap();
            tokio::time::sleep(Duration::from_millis(100)).await;
        });
        let (tx, mut rx) = mpsc::channel(2);
        let (cancel, cancel_rx) = watch::channel(false);
        let validity = Arc::new(AtomicU64::new(0));
        let worker = tokio::spawn(run(cfg, tx, cancel_rx, validity.clone()));
        assert!(matches!(receive(&mut rx).await, Event::Snapshot(_, _)));
        server.await.unwrap();
        assert!(rx.try_recv().is_err());
        let _ = cancel.send(true);
        assert!(matches!(
            worker.await.unwrap(),
            Err(SourceError::Transport | SourceError::Cancelled)
        ));
        assert_eq!(validity.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn unanswered_application_ping_expires_and_revokes_epoch() {
        let (listener, cfg) = listener().await;
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut ws = accept_async(stream).await.unwrap();
            let _ = ws.next().await;
            ws.send(Message::Text(ack().into())).await.unwrap();
            ws.send(Message::Text(book("snapshot", 1, -1).to_string().into()))
                .await
                .unwrap();
            let ping = ws.next().await.unwrap().unwrap();
            assert_eq!(ping.to_text().unwrap(), "ping");
            let _ = ws.next().await;
        });
        let (tx, mut rx) = mpsc::channel(2);
        let (_cancel, cancel_rx) = watch::channel(false);
        let validity = Arc::new(AtomicU64::new(0));
        let worker = tokio::spawn(run(cfg, tx, cancel_rx, validity.clone()));
        assert!(matches!(receive(&mut rx).await, Event::Snapshot(_, _)));
        assert_eq!(
            timeout(PING_AFTER + PONG_WAIT + Duration::from_secs(3), worker)
                .await
                .unwrap()
                .unwrap(),
            Err(SourceError::Timeout)
        );
        assert_eq!(validity.load(Ordering::SeqCst), 0);
        server.abort();
    }

    #[tokio::test]
    async fn cancelled_before_connect_never_retains_validity() {
        let (tx, _rx) = mpsc::channel(1);
        let (_cancel, cancel_rx) = watch::channel(true);
        let validity = Arc::new(AtomicU64::new(9));
        assert_eq!(
            run(config(), tx, cancel_rx, validity.clone()).await,
            Err(SourceError::Cancelled)
        );
        assert_eq!(validity.load(Ordering::SeqCst), 0);
    }
}

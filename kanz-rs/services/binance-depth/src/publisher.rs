//! Canonical FACT framing. Only the two public market-data subjects exist here.
use crate::proto::{
    envelope::v1::{Envelope, EventFrame},
    market::v1::{market_data_event, MarketDataEvent, OrderBookSnapshot, Quote},
};
use kanz_decimal_compat::{compare, in_domain, is_positive};
use prost::Message;
use prost_types::Timestamp;
use std::{
    cmp::Ordering,
    error::Error,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use uuid::Uuid;

pub const BOOK_SUBJECT: &str = "market.book.snapshot";
pub const QUOTE_SUBJECT: &str = "market.crypto.quote";
type Result<T> = std::result::Result<T, Box<dyn Error + Send + Sync>>;

pub fn now() -> Timestamp {
    let duration = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default();
    Timestamp {
        seconds: duration.as_secs() as i64,
        nanos: duration.subsec_nanos() as i32,
    }
}

pub fn quote(snapshot: &OrderBookSnapshot) -> Option<MarketDataEvent> {
    let bid = snapshot.bids.first()?;
    let ask = snapshot.asks.first()?;
    snapshot.event_time.as_ref()?;
    if !is_positive(bid.price.as_ref())
        || !is_positive(ask.price.as_ref())
        || !is_positive(bid.size.as_ref())
        || !is_positive(ask.size.as_ref())
        || compare(ask.price.as_ref(), bid.price.as_ref()) != Ordering::Greater
    {
        return None;
    }
    Some(MarketDataEvent {
        instrument_id: snapshot.instrument_id.clone(),
        symbol: snapshot.symbol.clone(),
        mic: snapshot.mic.clone(),
        event_time: snapshot.event_time,
        source_sequence: snapshot.last_update_sequence,
        data: Some(market_data_event::Data::Quote(Quote {
            bid_price: bid.price,
            bid_size: bid.size,
            ask_price: ask.price,
            ask_size: ask.size,
            conditions: vec![],
        })),
    })
}

pub struct Framer {
    tenant: String,
    source: String,
    version: String,
    instrument: String,
    sequence: [u64; 2],
}

impl Framer {
    pub fn new(
        tenant: String,
        source: String,
        version: String,
        instrument: String,
    ) -> Result<Self> {
        if [&tenant, &source, &version, &instrument]
            .iter()
            .any(|s| s.is_empty() || s.len() > 256 || s.chars().any(char::is_control))
            || !source.contains('/')
        {
            return Err(
                "tenant, service/instance source, version and instrument are required".into(),
            );
        }
        Ok(Self {
            tenant,
            source,
            version,
            instrument,
            sequence: [0; 2],
        })
    }

    pub fn frames(
        &mut self,
        snapshot: &OrderBookSnapshot,
        published: Timestamp,
    ) -> Result<Vec<(&'static str, EventFrame)>> {
        if snapshot.instrument_id != self.instrument
            || snapshot.mic != "BINANCE"
            || snapshot.symbol.is_empty()
        {
            return Err("book identity does not match this single-instrument publisher".into());
        }
        if snapshot
            .bids
            .iter()
            .chain(snapshot.asks.iter())
            .any(|level| {
                level.price.is_none()
                    || level.size.is_none()
                    || !in_domain(level.price.as_ref())
                    || !in_domain(level.size.as_ref())
            })
        {
            return Err("book levels require canonical-domain price and size".into());
        }
        let venue_time = snapshot.event_time.ok_or("book has no venue time")?;
        if venue_time.seconds <= 0
            || venue_time.seconds > 253402300799
            || !(0..1_000_000_000).contains(&venue_time.nanos)
            || published.seconds <= 0
            || published.seconds > 253402300799
            || !(0..1_000_000_000).contains(&published.nanos)
        {
            return Err("invalid venue or publication timestamp".into());
        }
        let mut frames = Vec::with_capacity(2);
        frames.push((
            BOOK_SUBJECT,
            self.frame(
                0,
                BOOK_SUBJECT,
                "market.v1.OrderBookSnapshot:1",
                snapshot.encode_to_vec(),
                venue_time,
                published,
            )?,
        ));
        if let Some(quote) = quote(snapshot) {
            frames.push((
                QUOTE_SUBJECT,
                self.frame(
                    1,
                    QUOTE_SUBJECT,
                    "market.v1.MarketDataEvent:1",
                    quote.encode_to_vec(),
                    venue_time,
                    published,
                )?,
            ));
        }
        Ok(frames)
    }

    fn frame(
        &mut self,
        slot: usize,
        subject: &str,
        schema: &str,
        payload: Vec<u8>,
        venue_time: Timestamp,
        published: Timestamp,
    ) -> Result<EventFrame> {
        self.sequence[slot] = self.sequence[slot]
            .checked_add(1)
            .ok_or("producer sequence exhausted")?;
        let id = Uuid::now_v7().to_string();
        Ok(EventFrame {
            envelope: Some(Envelope {
                event_id: id.clone(),
                event_type: subject.into(),
                schema_version: 1,
                envelope_version: 2,
                event_class: crate::proto::envelope::v1::EventClass::Fact as i32,
                domain: "market".into(),
                event_time: Some(venue_time),
                ingestion_time: Some(published),
                publish_time: Some(published),
                correlation_id: id.clone(),
                causation_id: String::new(),
                trace_context: String::new(),
                source: self.source.clone(),
                producer_version: self.version.clone(),
                partition_key: self.instrument.clone(),
                producer_sequence: self.sequence[slot],
                idempotency_key: id,
                quality_flags: vec![],
                payload_schema_ref: schema.into(),
                tenant_id: self.tenant.clone(),
            }),
            payload,
        })
    }
}

pub async fn publish(
    js: &async_nats::jetstream::Context,
    frames: Vec<(&'static str, EventFrame)>,
    validity: &std::sync::atomic::AtomicU64,
    epoch: u64,
) -> Result<()> {
    let mut failed = false;
    for (subject, frame) in frames {
        if !crate::source::valid_epoch(validity, epoch) {
            return Err("source generation invalidated before publication".into());
        }
        let mut headers = async_nats::HeaderMap::new();
        let id = &frame
            .envelope
            .as_ref()
            .ok_or("missing envelope")?
            .idempotency_key;
        headers.insert("Nats-Msg-Id", id.as_str());
        headers.insert(
            "Kanz-Partition-Key",
            frame
                .envelope
                .as_ref()
                .ok_or("missing envelope")?
                .partition_key
                .as_str(),
        );
        // Await the durable JetStream ACK, not just enqueueing into the client.
        let result = guarded_ack(validity, epoch, async {
            tokio::time::timeout(Duration::from_secs(5), async {
                js.publish_with_headers(subject, headers, frame.encode_to_vec().into())
                    .await?
                    .await?;
                Ok::<(), Box<dyn Error + Send + Sync>>(())
            })
            .await??;
            Ok(())
        })
        .await;
        if !crate::source::valid_epoch(validity, epoch) {
            return Err("source generation invalidated during publication".into());
        }
        if result.is_err() {
            failed = true;
        }
    }
    if failed {
        return Err("market FACT publish was not acknowledged".into());
    }
    Ok(())
}

// The transport future is lazy: invalid generations never start another send.
async fn guarded_ack(
    validity: &std::sync::atomic::AtomicU64,
    epoch: u64,
    send_and_ack: impl std::future::Future<Output = Result<()>>,
) -> Result<()> {
    if !crate::source::valid_epoch(validity, epoch) {
        return Err("source invalid before send".into());
    }
    send_and_ack.await?;
    if !crate::source::valid_epoch(validity, epoch) {
        return Err("source invalid after ACK".into());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{decimal::parse, proto::market::v1::PriceLevel};
    #[tokio::test]
    async fn invalidation_during_blocked_ack_prevents_next_frame() {
        use std::sync::atomic::{AtomicU64, Ordering};
        // Zero models gap/close; a different nonzero epoch models recovery
        // finishing before the old ACK returns. Neither revives the old pair.
        for replacement in [0, 2] {
            let validity = AtomicU64::new(1);
            let starts = AtomicU64::new(0);
            let (ack, wait) = tokio::sync::oneshot::channel();
            let blocked = guarded_ack(&validity, 1, async {
                starts.fetch_add(1, Ordering::SeqCst);
                wait.await?;
                Ok(())
            });
            let invalidate = async {
                while starts.load(Ordering::SeqCst) == 0 {
                    tokio::task::yield_now().await;
                }
                validity.store(replacement, Ordering::SeqCst);
                ack.send(()).unwrap();
            };
            let (result, ()) = tokio::join!(blocked, invalidate);
            assert!(result.is_err());
            assert!(guarded_ack(&validity, 1, async {
                starts.fetch_add(1, Ordering::SeqCst);
                Ok(())
            })
            .await
            .is_err());
            assert_eq!(starts.load(Ordering::SeqCst), 1);
        }
    }
    fn snapshot() -> OrderBookSnapshot {
        OrderBookSnapshot {
            instrument_id: "BTC-USDT".into(),
            symbol: "BTCUSDT".into(),
            mic: "BINANCE".into(),
            event_time: Some(Timestamp {
                seconds: 1700000000,
                nanos: 123,
            }),
            last_update_sequence: 23,
            bids: vec![PriceLevel {
                price: parse("100"),
                size: parse("2"),
            }],
            asks: vec![PriceLevel {
                price: parse("101"),
                size: parse("3"),
            }],
        }
    }
    #[test]
    fn canonical_pair_preserves_venue_clock_and_separate_sequences() {
        let snapshot = snapshot();
        let mut framer = Framer::new(
            "t".into(),
            "binance-depth/test".into(),
            "sha".into(),
            "BTC-USDT".into(),
        )
        .unwrap();
        let frames = framer.frames(&snapshot, now()).unwrap();
        assert_eq!(frames.len(), 2);
        for (subject, frame) in &frames {
            let env = frame.envelope.as_ref().unwrap();
            assert_eq!(env.event_type, *subject);
            assert_eq!(env.event_id, env.idempotency_key);
            assert_eq!(env.event_time, snapshot.event_time);
            assert_eq!(env.tenant_id, "t");
            assert_eq!(env.producer_sequence, 1);
            assert_eq!(Uuid::parse_str(&env.event_id).unwrap().get_version_num(), 7);
        }
        let event = MarketDataEvent::decode(frames[1].1.payload.as_slice()).unwrap();
        assert!(matches!(
            event.data,
            Some(market_data_event::Data::Quote(_))
        ));
        assert_eq!(event.source_sequence, 23);
        assert_eq!(
            framer.frames(&snapshot, now()).unwrap()[0]
                .1
                .envelope
                .as_ref()
                .unwrap()
                .producer_sequence,
            2
        );
    }
    #[test]
    fn unknown_or_crossed_market_is_not_a_quote() {
        let mut snap = snapshot();
        snap.asks.clear();
        assert!(quote(&snap).is_none());
        let mut snap = snapshot();
        snap.asks[0].price = parse("100");
        assert!(quote(&snap).is_none());
        snap.asks[0].price = parse("99");
        assert!(quote(&snap).is_none());
        let mut snap = snapshot();
        snap.event_time = None;
        assert!(quote(&snap).is_none());
        let mut snap = snapshot();
        snap.bids[0].size = None;
        assert!(quote(&snap).is_none());
    }
    #[test]
    fn required_identity_and_time_fail_closed() {
        assert!(Framer::new("".into(), "s/i".into(), "v".into(), "BTC-USDT".into()).is_err());
        let mut framer =
            Framer::new("t".into(), "s/i".into(), "v".into(), "BTC-USDT".into()).unwrap();
        let mut snap = snapshot();
        snap.instrument_id = "OTHER".into();
        assert!(framer.frames(&snap, now()).is_err());
        snap = snapshot();
        snap.event_time = None;
        assert!(framer.frames(&snap, now()).is_err());
        snap = snapshot();
        snap.bids[0].price.as_mut().unwrap().exponent = 65;
        assert!(framer.frames(&snap, now()).is_err());
    }
}

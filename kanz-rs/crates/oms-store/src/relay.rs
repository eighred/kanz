//! Existing OMS relay algorithm, with no connector, credentials or production caller.
//! The caller owns transport and must await Run before closing its dependencies.
use crate::{outbox, Error, Record, MAX_BYTES};
use kanz_order_core::proto::{
    envelope::v1::{Envelope, EventClass, EventFrame},
    FILE_DESCRIPTOR_SET,
};
use prost::Message;
use prost_reflect::DescriptorPool;
use std::{
    collections::HashMap,
    future::Future,
    pin::Pin,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};
use tokio_postgres::Client;

pub type Task<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

/// A successful result means the broker acknowledged persistence, not socket write.
/// This seam carries only an already reconstructed canonical FACT frame.
pub trait Publisher {
    fn publish<'a>(&'a mut self, frame: &'a Frame) -> Task<'a, Result<(), PublishFailure>>;
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PublishFailure {
    InvalidRecord,
    Broker,
}
impl PublishFailure {
    fn cause(self) -> &'static str {
        match self {
            Self::InvalidRecord => "relay: invalid canonical FACT",
            Self::Broker => "relay: broker publication unconfirmed",
        }
    }
}
pub trait Sessions {
    fn connect(&mut self) -> Task<'_, Result<outbox::Session, Error>>;
}
pub struct Frame {
    pub subject: String,
    pub partition_key: String,
    pub event_id: String,
    pub body: Vec<u8>,
}

/// Stamping mirrors Go Producer's per-attempt identity and per-key sequence.
/// The descriptor set is compiled from canonical schemas; absent schemas refuse.
pub struct Framer {
    pool: DescriptorPool,
    source: String,
    version: String,
    sequence: HashMap<(String, String), (u64, Instant)>,
    sequence_bytes: usize,
}
impl Framer {
    pub fn new(source: String, version: String) -> Result<Self, Error> {
        if source.is_empty() || version.is_empty() || source.len() + version.len() > MAX_BYTES {
            return Err(Error::InvalidInput);
        }
        Ok(Self {
            pool: DescriptorPool::decode(FILE_DESCRIPTOR_SET).map_err(|_| Error::InvalidInput)?,
            source,
            version,
            sequence: HashMap::new(),
            sequence_bytes: 0,
        })
    }
    pub fn frame(&mut self, r: &Record) -> Result<Frame, PublishFailure> {
        let bad = PublishFailure::InvalidRecord;
        if r.event_class != EventClass::Fact as i32
            || r.schema_version <= 0
            || r.event_type.is_empty()
            || r.domain.is_empty()
            || r.tenant.is_empty()
            || r.payload_schema_ref.is_empty()
        {
            return Err(bad);
        }
        let bytes = [
            &r.tenant,
            &r.partition_key,
            &r.subject,
            &r.event_type,
            &r.domain,
            &r.payload_schema_ref,
            &r.correlation_id,
            &r.causation_id,
            &r.trace_context,
        ]
        .iter()
        .map(|v| v.len())
        .sum::<usize>();
        if bytes > MAX_BYTES || r.payload.len() > MAX_BYTES - bytes {
            return Err(bad);
        }
        let name = r
            .payload_schema_ref
            .rsplit_once(':')
            .map_or(r.payload_schema_ref.as_str(), |(n, _)| n);
        let desc = self.pool.get_message_by_name(name).ok_or(bad)?;
        let payload = crate::relay_wire::normalize(desc, &r.payload).map_err(|_| bad)?;
        let event_time = timestamp(r.event_time).ok_or(bad)?;
        if event_time.seconds == -62135596800 && event_time.nanos == 0 {
            return Err(bad);
        }
        let now = timestamp(SystemTime::now()).ok_or(bad)?;
        let id = uuid::Uuid::now_v7().to_string();
        let seq = if r.partition_key.is_empty() {
            0
        } else {
            let key = (r.event_type.clone(), r.partition_key.clone());
            let cost = key.0.len() + key.1.len();
            let new = !self.sequence.contains_key(&key);
            if new && (self.sequence.len() >= 4096 || self.sequence_bytes + cost > 8 * MAX_BYTES) {
                self.sequence
                    .retain(|_, (_, at)| at.elapsed() < Duration::from_secs(3600));
                self.sequence_bytes = self.sequence.keys().map(|(a, b)| a.len() + b.len()).sum();
                while self.sequence.len() >= 4096 || self.sequence_bytes + cost > 8 * MAX_BYTES {
                    if let Some(oldest) = self
                        .sequence
                        .iter()
                        .min_by_key(|(_, (_, at))| *at)
                        .map(|(k, _)| k.clone())
                    {
                        self.sequence_bytes -= oldest.0.len() + oldest.1.len();
                        self.sequence.remove(&oldest);
                    }
                }
            }
            if new {
                self.sequence_bytes += cost;
            }
            let value = self.sequence.entry(key).or_insert((0, Instant::now()));
            value.0 = value.0.checked_add(1).ok_or(bad)?;
            value.1 = Instant::now();
            value.0
        };
        let env = Envelope {
            event_id: id.clone(),
            event_type: r.event_type.clone(),
            schema_version: r.schema_version as u32,
            envelope_version: 2,
            event_class: r.event_class,
            domain: r.domain.clone(),
            event_time: Some(event_time),
            ingestion_time: Some(now),
            publish_time: Some(now),
            correlation_id: if r.correlation_id.is_empty() {
                id.clone()
            } else {
                r.correlation_id.clone()
            },
            causation_id: r.causation_id.clone(),
            trace_context: r.trace_context.clone(),
            source: self.source.clone(),
            producer_version: self.version.clone(),
            partition_key: r.partition_key.clone(),
            producer_sequence: seq,
            idempotency_key: id.clone(),
            quality_flags: vec![],
            payload_schema_ref: r.payload_schema_ref.clone(),
            tenant_id: r.tenant.clone(),
        };
        let body = EventFrame {
            envelope: Some(env),
            payload,
        }
        .encode_to_vec();
        if body.len() > MAX_BYTES {
            return Err(bad);
        }
        Ok(Frame {
            subject: r.subject.clone(),
            partition_key: r.partition_key.clone(),
            event_id: id,
            body,
        })
    }
}
fn timestamp(t: SystemTime) -> Option<prost_types::Timestamp> {
    let (seconds, nanos) = match t.duration_since(UNIX_EPOCH) {
        Ok(d) => (i64::try_from(d.as_secs()).ok()?, d.subsec_nanos() as i32),
        Err(e) => {
            let d = e.duration();
            let s = i64::try_from(d.as_secs()).ok()?;
            if d.subsec_nanos() == 0 {
                (-s, 0)
            } else {
                (-s - 1, 1_000_000_000 - d.subsec_nanos() as i32)
            }
        }
    };
    (-62135596800..=253402300799)
        .contains(&seconds)
        .then_some(prost_types::Timestamp { seconds, nanos })
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Stall {
    Stopped,
    Contended,
    Publication(PublishFailure),
    FullBatch,
}
#[derive(Debug)]
pub struct Outcome {
    pub sent: usize,
    pub stalled: Option<Stall>,
}
/// Infrastructure errors preserve how many records were already marked published.
#[derive(Debug)]
pub struct Failure {
    pub sent: usize,
    pub error: Error,
}
pub struct Relay<P, S> {
    publisher: P,
    sessions: S,
    framer: Framer,
    tenant: String,
    batch: i64,
    interval: Duration,
    stop: std::sync::Arc<std::sync::atomic::AtomicBool>,
}
impl<P: Publisher, S: Sessions> Relay<P, S> {
    pub fn new(
        publisher: P,
        sessions: S,
        framer: Framer,
        tenant: String,
        batch: i64,
        interval: Duration,
    ) -> Result<Self, Error> {
        if tenant.is_empty()
            || tenant.len() > MAX_BYTES
            || !(1..=outbox::MAX_PAGE).contains(&batch)
            || interval.is_zero()
            || interval > Duration::from_secs(3600)
        {
            return Err(Error::InvalidInput);
        }
        Ok(Self {
            publisher,
            sessions,
            framer,
            tenant,
            batch,
            interval,
            stop: std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false)),
        })
    }
    pub fn stop_handle(&self) -> std::sync::Arc<std::sync::atomic::AtomicBool> {
        self.stop.clone()
    }
    fn stopped(&self) -> bool {
        self.stop.load(std::sync::atomic::Ordering::Acquire)
    }
    pub async fn flush(&mut self, key: &str) -> Result<Outcome, Failure> {
        self.drain_key(key, true).await
    }
    /// One absolute work deadline; cleanup is separately awaited. Only confirmed
    /// published-and-marked records contribute to sent. An expired suffix remains
    /// uncertain, even if its broker acknowledgement already arrived.
    pub async fn flush_before(
        &mut self,
        key: &str,
        expires: tokio::time::Instant,
    ) -> Result<Outcome, Failure> {
        work_remaining(Some(expires), 0)?;
        let connected = tokio::time::timeout_at(expires, self.sessions.connect()).await;
        let session = match connected {
            Ok(Ok(session)) => session,
            Ok(Err(error)) => return Err(Failure { sent: 0, error }),
            Err(_) => {
                return Err(Failure {
                    sent: 0,
                    error: Error::PublicationUnconfirmed,
                })
            }
        };
        if tokio::time::Instant::now() >= expires {
            session.close().await;
            return Err(Failure {
                sent: 0,
                error: Error::PublicationUnconfirmed,
            });
        }
        let Some(mut lock) = outbox::lock_key_before(session, &self.tenant, key, expires)
            .await
            .map_err(|error| Failure { sent: 0, error })?
        else {
            return Ok(Outcome {
                sent: 0,
                stalled: Some(Stall::Contended),
            });
        };
        let mut confirmed = 0;
        let result = tokio::time::timeout_at(
            expires,
            self.under_lock(lock.client(), key, &mut confirmed, Some(expires)),
        )
        .await;
        if tokio::time::Instant::now() >= expires || result.is_err() {
            lock.close().await;
            return Err(Failure {
                sent: confirmed,
                error: Error::PublicationUnconfirmed,
            });
        }
        let result = result.expect("checked timeout result");
        match lock.release().await {
            Ok(session) => session.close().await,
            Err(error) => {
                return Err(Failure {
                    sent: confirmed,
                    error,
                })
            }
        }
        if tokio::time::Instant::now() >= expires {
            return Err(Failure {
                sent: confirmed,
                error: Error::PublicationUnconfirmed,
            });
        }
        result
    }
    pub async fn drain_once(&mut self, client: &mut Client) -> Result<usize, Failure> {
        let keys = outbox::pending_keys(client, &self.tenant, self.batch)
            .await
            .map_err(|error| Failure { sent: 0, error })?;
        let mut sent = 0;
        for key in keys {
            if self.stopped() {
                break;
            }
            match self.drain_key(&key, false).await {
                Ok(out) => sent += out.sent,
                Err(mut failure) => {
                    failure.sent += sent;
                    return Err(failure);
                }
            }
        }
        Ok(sent)
    }
    /// Stop is checked between owned passes. Publication futures must be bounded
    /// by the connector. No spawned drain can outlive this awaited method.
    pub async fn run(
        &mut self,
        client: &mut Client,
        mut observe: impl FnMut(&Result<usize, Failure>),
    ) {
        while !self.stopped() {
            let result = self.drain_once(client).await;
            observe(&result);
            let deadline = Instant::now() + self.interval;
            while !self.stopped() && Instant::now() < deadline {
                tokio::time::sleep(
                    deadline
                        .saturating_duration_since(Instant::now())
                        .min(Duration::from_millis(10)),
                )
                .await;
            }
        }
    }
    async fn drain_key(&mut self, key: &str, wait: bool) -> Result<Outcome, Failure> {
        let session = self
            .sessions
            .connect()
            .await
            .map_err(|error| Failure { sent: 0, error })?;
        let Some(mut lock) = outbox::lock_key(session, &self.tenant, key, wait)
            .await
            .map_err(|error| Failure { sent: 0, error })?
        else {
            return Ok(Outcome {
                sent: 0,
                stalled: Some(Stall::Contended),
            });
        };
        let mut confirmed = 0;
        let result = self
            .under_lock(lock.client(), key, &mut confirmed, None)
            .await;
        // Release even after decode, publish, read or mark failure. Never return a
        // connection to a pool, and join the dedicated session before returning.
        match lock.release().await {
            Ok(session) => {
                session.close().await;
                result
            }
            Err(error) => Err(Failure {
                sent: result.as_ref().map_or_else(|e| e.sent, |v| v.sent),
                error,
            }),
        }
    }
    async fn under_lock(
        &mut self,
        client: &mut Client,
        key: &str,
        confirmed: &mut usize,
        expires: Option<tokio::time::Instant>,
    ) -> Result<Outcome, Failure> {
        work_remaining(expires, 0)?;
        // Keep the whole-page fast path. Capacity alone permits bounded single-row
        // reads under the same key lock; Queue::pending itself never returns a prefix.
        // A later bad row can therefore follow acknowledged publications, but its
        // unresolved suffix always returns failure rather than an empty-queue proof.
        let (records, streaming) =
            match outbox::pending(client, &self.tenant, key, self.batch).await {
                Ok(records) => (records, false),
                Err(Error::Capacity) => (Vec::new(), true),
                Err(error) => return Err(Failure { sent: 0, error }),
            };
        work_remaining(expires, 0)?;
        let full = streaming || records.len() == self.batch as usize;
        let mut records = records.into_iter();
        let mut sent = 0;
        while sent < self.batch as usize {
            work_remaining(expires, sent)?;
            let next = if streaming {
                outbox::pending(client, &self.tenant, key, 1)
                    .await
                    .map_err(|error| Failure { sent, error })?
                    .into_iter()
                    .next()
            } else {
                records.next()
            };
            work_remaining(expires, sent)?;
            let Some(p) = next else {
                return Ok(Outcome {
                    sent,
                    stalled: None,
                });
            };
            if self.stopped() {
                return Ok(Outcome {
                    sent,
                    stalled: Some(Stall::Stopped),
                });
            }
            let published = match self.framer.frame(&p.record) {
                Ok(frame) => {
                    work_remaining(expires, sent)?;
                    tokio::time::timeout(Duration::from_secs(10), self.publisher.publish(&frame))
                        .await
                        .unwrap_or(Err(PublishFailure::Broker))
                }
                Err(e) => Err(e),
            };
            work_remaining(expires, sent)?;
            if let Err(failure) = published {
                outbox::mark_failed(client, &self.tenant, p.id, Some(failure.cause()))
                    .await
                    .map_err(|error| Failure { sent, error })?;
                return Ok(Outcome {
                    sent,
                    stalled: Some(Stall::Publication(failure)),
                });
            }
            outbox::mark_published(client, &self.tenant, p.id)
                .await
                .map_err(|error| Failure { sent, error })?;
            sent += 1;
            *confirmed = sent;
            work_remaining(expires, sent)?;
        }
        Ok(Outcome {
            sent,
            stalled: full.then_some(Stall::FullBatch),
        })
    }
}

// A timeout future can poll an already-ready inner future first. Check between
// side effects as well as after the outer future so expiry never starts new work.
fn work_remaining(expires: Option<tokio::time::Instant>, sent: usize) -> Result<(), Failure> {
    if expires.is_some_and(|at| tokio::time::Instant::now() >= at) {
        Err(Failure {
            sent,
            error: Error::PublicationUnconfirmed,
        })
    } else {
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[tokio::test]
    async fn expired_ready_inner_preserves_confirmed_prefix_and_refuses_success() {
        let expires = tokio::time::Instant::now() - Duration::from_secs(1);
        let ready = tokio::time::timeout_at(expires, std::future::ready(Ok::<_, Error>(()))).await;
        assert!(matches!(ready, Ok(Ok(()))));
        let refusal = work_remaining(Some(expires), 3).unwrap_err();
        assert_eq!(refusal.sent, 3);
        assert_eq!(refusal.error, Error::PublicationUnconfirmed);
        assert!(work_remaining(None, 3).is_ok());
        assert!(work_remaining(
            Some(tokio::time::Instant::now() + Duration::from_secs(1)),
            3
        )
        .is_ok());
    }
    #[tokio::test]
    async fn already_expired_flush_does_not_open_a_session() {
        struct NeverPublisher;
        impl Publisher for NeverPublisher {
            fn publish<'a>(&'a mut self, _: &'a Frame) -> Task<'a, Result<(), PublishFailure>> {
                panic!("expired work reached publisher")
            }
        }
        struct CountSessions(std::sync::Arc<std::sync::atomic::AtomicUsize>);
        impl Sessions for CountSessions {
            fn connect(&mut self) -> Task<'_, Result<outbox::Session, Error>> {
                self.0.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                Box::pin(async { Err(Error::Database) })
            }
        }
        let calls = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let mut relay = Relay::new(
            NeverPublisher,
            CountSessions(calls.clone()),
            Framer::new("fixture".into(), "version".into()).unwrap(),
            "tenant".into(),
            1,
            Duration::from_millis(1),
        )
        .unwrap();
        let result = relay
            .flush_before("key", tokio::time::Instant::now() - Duration::from_secs(1))
            .await
            .unwrap_err();
        assert_eq!(result.sent, 0);
        assert_eq!(result.error, Error::PublicationUnconfirmed);
        assert_eq!(calls.load(std::sync::atomic::Ordering::SeqCst), 0);
    }
    fn record() -> Record {
        Record {
            tenant: "tenant-a".into(),
            partition_key: "order-a".into(),
            subject: "order.order.rejected".into(),
            event_type: "order.rejected".into(),
            event_class: EventClass::Fact as i32,
            schema_version: 1,
            domain: "order".into(),
            payload_schema_ref: "order.v1.OrderRejected:1".into(),
            event_time: UNIX_EPOCH + Duration::from_secs(1_700_000_000),
            correlation_id: "correlation".into(),
            causation_id: "cause".into(),
            trace_context: "trace".into(),
            payload: vec![10, 1, b'a'],
        }
    }
    #[test]
    fn retry_keeps_payload_lineage_but_has_new_go_style_identity() {
        let mut f = Framer::new("oms/test".into(), "version".into()).unwrap();
        let r = record();
        let a = f.frame(&r).unwrap();
        let b = f.frame(&r).unwrap();
        assert_ne!(a.event_id, b.event_id);
        let a = EventFrame::decode(a.body.as_slice()).unwrap();
        let b = EventFrame::decode(b.body.as_slice()).unwrap();
        assert_eq!(a.payload, r.payload);
        assert_eq!(a.payload, b.payload);
        let a = a.envelope.unwrap();
        let b = b.envelope.unwrap();
        assert_eq!(a.producer_sequence, 1);
        assert_eq!(b.producer_sequence, 2);
        assert_eq!(a.idempotency_key, a.event_id);
        assert_eq!(a.tenant_id, r.tenant);
        assert_eq!(a.correlation_id, r.correlation_id);
        assert_eq!(a.causation_id, r.causation_id);
        assert_eq!(a.trace_context, r.trace_context);
    }
    #[test]
    fn command_outcome_descriptor_is_in_the_live_fact_domain() {
        let mut f = Framer::new("oms/test".into(), "version".into()).unwrap();
        let mut r = record();
        r.payload_schema_ref = "command.v1.CommandOutcome:1".into();
        r.payload = vec![8, 1, 18, 1, b'a'];
        let frame = f.frame(&r).unwrap();
        assert_eq!(
            EventFrame::decode(frame.body.as_slice()).unwrap().payload,
            r.payload
        );
    }
    #[test]
    fn missing_schema_tenant_class_or_valid_timestamp_refuses() {
        let mut f = Framer::new("oms/test".into(), "version".into()).unwrap();
        let mut r = record();
        r.payload_schema_ref = "missing.Type:1".into();
        assert!(f.frame(&r).is_err());
        let mut r = record();
        r.tenant.clear();
        assert!(f.frame(&r).is_err());
        let mut r = record();
        r.event_class = 0;
        assert!(f.frame(&r).is_err());
        let mut r = record();
        r.event_time = UNIX_EPOCH - Duration::from_secs(62135596800);
        assert!(f.frame(&r).is_err());
        let mut r = record();
        r.payload = vec![0; MAX_BYTES + 1];
        assert!(f.frame(&r).is_err());
    }
    #[test]
    fn sequence_key_storage_is_bounded() {
        let mut f = Framer::new("oms/test".into(), "version".into()).unwrap();
        let mut r = record();
        for n in 0..20 {
            r.partition_key = format!("{}-{n}", "x".repeat(MAX_BYTES / 2));
            f.frame(&r).unwrap();
        }
        let bytes: usize = f.sequence.keys().map(|(a, b)| a.len() + b.len()).sum();
        assert!(bytes <= 8 * MAX_BYTES);
        assert_eq!(bytes, f.sequence_bytes);
    }
}

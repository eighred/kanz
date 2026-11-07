//! Concrete existing recovery/fee-handler composition. Transport seams carry SQL sessions and
//! already committed FACTs only; neither seam can authorize a financial action.
use crate::{fee_commands, handlers, observation, Error};
use kanz_oms_store::diagnostics::{measure, Stage};
use kanz_oms_store::relay::{Framer, Publisher, Relay, Sessions};
use kanz_oms_venue::{Client, Requirements};
use std::time::Duration;

pub struct Config {
    pub venue: kanz_oms_venue::Config,
    pub portfolio: String,
    pub mic: String,
    pub account: String,
}
#[derive(Clone, Copy)]
pub enum Method {
    Observation,
    Ack,
    Parked,
    FeeCorrection,
}
pub struct Coordinator<P, S> {
    tenant: String,
    binding: observation::Binding,
    venue: Client,
    sessions: S,
    relay: Relay<P, S>,
}
impl<P: Publisher, S: Sessions + Clone> Coordinator<P, S> {
    /// Startup uses the sole rotating SPIFFE/h2 connector and real Describe plus
    /// ListInstruments registration. No caller-supplied Channel or permit exists.
    pub async fn connect(
        config: Config,
        sessions: S,
        publisher: P,
        framer: Framer,
        deadline: Duration,
    ) -> Result<Self, Error> {
        if deadline.is_zero()
            || deadline > Duration::from_secs(120)
            || [
                &config.portfolio,
                &config.mic,
                &config.account,
                &config.venue.tenant,
            ]
            .iter()
            .any(|v| !kanz_oms_store::recovery_repository::text(v, 256))
        {
            return Err(Error::InvalidInput);
        }
        let expires = tokio::time::Instant::now() + deadline;
        let tenant = config.venue.tenant.clone();
        let (stop, receiver) = tokio::sync::watch::channel(false);
        let connect = Client::connect_until(config.venue, receiver);
        tokio::pin!(connect);
        let connected = tokio::select! {
            result=&mut connect=>result,
            _=tokio::time::sleep_until(expires)=>{
                let _=stop.send(true);
                if let Ok(venue)=connect.await { venue.close().await; }
                return Err(Error::Deadline);
            }
        };
        let mut venue = connected.map_err(|_| {
            if tokio::time::Instant::now() >= expires {
                Error::Deadline
            } else {
                Error::Venue
            }
        })?;
        if tokio::time::Instant::now() >= expires {
            venue.close().await;
            return Err(Error::Deadline);
        }
        let registered = tokio::time::timeout_at(
            expires,
            venue.register(
                &config.mic,
                &config.account,
                Requirements {
                    verified_account: true,
                    declared_order_types: true,
                },
                expires.saturating_duration_since(tokio::time::Instant::now()),
            ),
        )
        .await;
        let expired = tokio::time::Instant::now() >= expires;
        if expired || !matches!(registered, Ok(Ok(_))) {
            venue.close().await;
            return Err(if expired || registered.is_err() {
                Error::Deadline
            } else {
                Error::Venue
            });
        }
        let relay = match Relay::new(
            publisher,
            sessions.clone(),
            framer,
            tenant.clone(),
            128,
            Duration::from_millis(100),
        ) {
            Ok(value) => value,
            Err(e) => {
                venue.close().await;
                return Err(e.into());
            }
        };
        if tokio::time::Instant::now() >= expires {
            venue.close().await;
            return Err(Error::Deadline);
        }
        Ok(Self {
            tenant: tenant.clone(),
            binding: observation::Binding {
                tenant: tenant.clone(),
                portfolio: config.portfolio,
                mic: config.mic,
                account: config.account,
            },
            venue,
            sessions,
            relay,
        })
    }
    /// A successful blocked handler is an acknowledgement, not a recovered book.
    /// The mutable owner serializes commands; database CAS controls other writers.
    pub async fn handle(
        &mut self,
        method: Method,
        envelope: &[u8],
        payload: &[u8],
        now: prost_types::Timestamp,
        deadline: Duration,
    ) -> Result<(), Error> {
        if deadline.is_zero()
            || deadline > Duration::from_secs(120)
            || !(-62135596800..=253402300799).contains(&now.seconds)
            || !(0..1_000_000_000).contains(&now.nanos)
        {
            return Err(Error::InvalidInput);
        }
        let expires = tokio::time::Instant::now() + deadline;
        let env = match method {
            Method::FeeCorrection => fee_commands::envelope(&self.tenant, envelope)?,
            _ => handlers::envelope(&self.tenant, envelope)?,
        };
        if tokio::time::Instant::now() >= expires {
            return Err(Error::Deadline);
        }
        let connected = tokio::time::timeout_at(
            expires,
            measure(Stage::HandlerConnect, self.sessions.connect()),
        )
        .await;
        let mut session = match connected {
            Ok(Ok(session)) => session,
            other => {
                if tokio::time::Instant::now() >= expires {
                    return Err(Error::Deadline);
                }
                return Err(match other {
                    Ok(Err(error)) => error.into(),
                    _ => Error::Deadline,
                });
            }
        };
        if tokio::time::Instant::now() >= expires {
            measure(Stage::HandlerClose, session.close()).await;
            return Err(Error::Deadline);
        }
        let phase = tokio::time::timeout_at(expires, async {
            let client = session.client();
            match method {
                Method::Observation => {
                    observation::observe(
                        client,
                        &self.venue,
                        &self.binding,
                        env.value(),
                        payload,
                        now,
                        expires.saturating_duration_since(tokio::time::Instant::now()),
                    )
                    .await
                }
                Method::Ack => handlers::ack(client, &self.tenant, env.value(), payload, now).await,
                Method::Parked => {
                    handlers::parked(client, &self.tenant, env.value(), payload, now).await
                }
                Method::FeeCorrection => {
                    fee_commands::handle(client, &self.tenant, env.value(), payload, now).await
                }
            }
        })
        .await;
        let phase_expired = tokio::time::Instant::now() >= expires;
        // The work clock continues during cleanup. A timed-out transaction may
        // have committed: Deadline is not a rollback receipt. Retain explicit
        // session ownership until cancellation and the driver have been joined.
        measure(Stage::HandlerClose, session.close()).await;
        let key = match phase {
            Ok(Ok(key)) => key,
            Ok(Err(error)) => {
                return Err(if phase_expired {
                    Error::Deadline
                } else {
                    error
                })
            }
            Err(_) => return Err(Error::Deadline),
        };
        // The successful handler may have committed financial state. Never begin
        // another phase after expiry or label its publication as confirmed.
        if tokio::time::Instant::now() >= expires {
            return Err(Error::PublicationUnconfirmed);
        }
        let result = self.relay.flush_before(&key, expires).await;
        match result {
            Ok(outcome) if outcome.stalled.is_none() => Ok(()),
            _ => Err(Error::PublicationUnconfirmed),
        }
    }
    /// Call only after owned Handle work has been joined. No detached handler or
    /// background financial task is created by this owner.
    pub async fn close(self) {
        self.venue.close().await;
    }
}

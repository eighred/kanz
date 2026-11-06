//! Concrete three-handler composition. Transport seams carry SQL sessions and
//! already committed FACTs only; neither seam can authorize a financial action.
use crate::{handlers, observation, Error};
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
        let tenant = config.venue.tenant.clone();
        let (stop, receiver) = tokio::sync::watch::channel(false);
        let connect = Client::connect_until(config.venue, receiver);
        tokio::pin!(connect);
        let mut venue = tokio::select! {
            result=&mut connect=>result.map_err(|_|Error::Venue)?,
            _=tokio::time::sleep(deadline)=>{
                let _=stop.send(true);
                if let Ok(venue)=connect.await { venue.close().await; }
                return Err(Error::Deadline);
            }
        };
        if venue
            .register(
                &config.mic,
                &config.account,
                Requirements {
                    verified_account: true,
                    declared_order_types: true,
                },
                deadline,
            )
            .await
            .is_err()
        {
            venue.close().await;
            return Err(Error::Venue);
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
        let env = handlers::envelope(&self.tenant, envelope)?;
        let mut session = tokio::time::timeout(deadline, self.sessions.connect())
            .await
            .map_err(|_| Error::Deadline)??;
        let phase = tokio::time::timeout(deadline, async {
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
                        deadline,
                    )
                    .await
                }
                Method::Ack => handlers::ack(client, &self.tenant, env.value(), payload, now).await,
                Method::Parked => {
                    handlers::parked(client, &self.tenant, env.value(), payload, now).await
                }
            }
        })
        .await;
        // Explicitly join the dedicated driver on every normal error/timeout.
        // Dropping the entire future retains Session's cancellation-safe fallback.
        session.close().await;
        let key = phase.map_err(|_| Error::Deadline)??;
        let result = tokio::time::timeout(deadline, self.relay.flush(&key))
            .await
            .map_err(|_| Error::PublicationUnconfirmed)?;
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

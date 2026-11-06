//! Sole network connector: rotating SPIFFE mTLS, exact peer and negotiated h2.
use crate::Client;
use hyper::rt::Executor;
use hyper_util::rt::TokioIo;
use std::{
    future::Future,
    pin::Pin,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{net::TcpStream, task::JoinHandle};
use tonic::{transport::Endpoint, Status};

pub struct Config {
    pub endpoint: String,
    pub workload_socket: String,
    pub expected_peer: String,
    pub tenant: String,
}
fn connection_host(uri: &http::Uri) -> Result<String, Status> {
    let host = uri
        .host()
        .ok_or_else(|| Status::invalid_argument("missing venue host"))?;
    // HTTP authorities bracket IPv6; socket and TLS IP parsers require the bare IP.
    // Keep the original authority on the HTTP/2 endpoint below.
    if let Some(inner) = host.strip_prefix('[').and_then(|h| h.strip_suffix(']')) {
        return inner
            .parse::<std::net::Ipv6Addr>()
            .map(|ip| ip.to_string())
            .map_err(|_| Status::invalid_argument("invalid venue host"));
    }
    Ok(host.to_owned())
}
#[derive(Default)]
struct Tasks {
    closed: bool,
    handles: Vec<JoinHandle<()>>,
}
#[derive(Clone, Default)]
struct OwnedExecutor(Arc<Mutex<Tasks>>);
impl Executor<Pin<Box<dyn Future<Output = ()> + Send>>> for OwnedExecutor {
    fn execute(&self, future: Pin<Box<dyn Future<Output = ()> + Send>>) {
        let mut tasks = self.0.lock().unwrap_or_else(|p| p.into_inner());
        tasks.handles.retain(|h| !h.is_finished());
        // Closed/bounded task ownership fails transport work rather than detaching it.
        if tasks.closed || tasks.handles.len() >= 64 {
            return;
        }
        tasks.handles.push(tokio::spawn(future));
    }
}
impl OwnedExecutor {
    fn abort(&self) -> Vec<JoinHandle<()>> {
        let mut tasks = self.0.lock().unwrap_or_else(|p| p.into_inner());
        tasks.closed = true;
        for task in &tasks.handles {
            task.abort();
        }
        std::mem::take(&mut tasks.handles)
    }
}
pub(crate) struct Owner {
    tasks: OwnedExecutor,
    source: Option<spiffe::X509Source>,
}
impl Drop for Owner {
    fn drop(&mut self) {
        // Explicit close joins; cancellation/drop aborts before any handle is detached.
        self.tasks.abort();
    }
}
impl Client {
    pub async fn connect(config: Config) -> Result<Self, Status> {
        let (_sender, stop) = tokio::sync::watch::channel(false);
        Self::connect_until(config, stop).await
    }
    /// Explicit startup cancellation retains and joins acquired identity/transport owners.
    pub async fn connect_until(
        config: Config,
        mut stop: tokio::sync::watch::Receiver<bool>,
    ) -> Result<Self, Status> {
        if config.endpoint.len() > 4096 || config.expected_peer.len() > 4096 {
            return Err(Status::invalid_argument("venue configuration bound"));
        }
        let uri: http::Uri = config
            .endpoint
            .parse()
            .map_err(|_| Status::invalid_argument("invalid venue endpoint"))?;
        if uri.scheme_str() != Some("https")
            || uri.authority().is_none()
            || uri.authority().is_some_and(|a| a.as_str().contains('@'))
            || uri.path_and_query().is_some_and(|p| p.as_str() != "/")
            || config.tenant.is_empty()
            || config.tenant.len() > 1024
            || !config.workload_socket.starts_with("unix://")
            || config.workload_socket.len() > 4096
        {
            return Err(Status::invalid_argument("invalid venue configuration"));
        }
        let peer: spiffe::SpiffeId = config
            .expected_peer
            .parse()
            .map_err(|_| Status::invalid_argument("invalid venue peer"))?;
        let domain = peer.trust_domain().clone();
        let host = connection_host(&uri)?;
        let port = uri.port_u16().unwrap_or(443);
        let server_name = rustls::pki_types::ServerName::try_from(host.clone())
            .map_err(|_| Status::invalid_argument("invalid venue host"))?;
        let authorizer = spiffe_rustls::authorizer::exact([config.expected_peer.as_str()])
            .map_err(|_| Status::invalid_argument("invalid venue peer"))?;
        // Tonic's internal HTTPS wrapper would apply TLS twice. This URI is private;
        // its only connector below returns authenticated TLS IO, never plaintext.
        let inner = format!(
            "http://{}",
            uri.authority()
                .ok_or_else(|| Status::invalid_argument("missing venue authority"))?
        );
        let endpoint = Endpoint::from_shared(inner)
            .map_err(|_| Status::invalid_argument("invalid venue endpoint"))?
            .connect_timeout(Duration::from_secs(5));
        if *stop.borrow() {
            return Err(Status::cancelled("venue connect cancelled"));
        }
        let source = tokio::select! {
            result=tokio::time::timeout(Duration::from_secs(10),spiffe::X509Source::builder().endpoint(&config.workload_socket).build()) => result
                .map_err(|_|Status::deadline_exceeded("workload identity deadline"))?
                .map_err(|_|Status::unauthenticated("workload identity unavailable"))?,
            _=stop.changed()=>return Err(Status::cancelled("venue connect cancelled")),
        };
        let mut owner = Owner {
            tasks: OwnedExecutor::default(),
            source: Some(source.clone()),
        };
        let mut tls = match spiffe_rustls::mtls_client(source)
            .trust_domain_policy(spiffe_rustls::LocalOnly(domain))
            .authorize(authorizer)
            .build()
        {
            Ok(tls) => tls,
            Err(_) => {
                owner.close().await;
                return Err(Status::unauthenticated(
                    "venue identity configuration refused",
                ));
            }
        };
        tls.alpn_protocols = vec![b"h2".to_vec()];
        let tls = tokio_rustls::TlsConnector::from(Arc::new(tls));
        let connector = tower::service_fn(move |_: http::Uri| {
            let host = host.clone();
            let tls = tls.clone();
            let server_name = server_name.clone();
            async move {
                let socket = TcpStream::connect((host.as_str(), port)).await?;
                let stream = tls.connect(server_name, socket).await?;
                if stream.get_ref().1.alpn_protocol() != Some(b"h2".as_slice()) {
                    return Err(std::io::Error::other("venue did not negotiate h2"));
                }
                Ok::<_, std::io::Error>(TokioIo::new(stream))
            }
        });
        let endpoint = endpoint.executor(owner.tasks.clone());
        let connection = tokio::select! {
            result=tokio::time::timeout(Duration::from_secs(10),endpoint.connect_with_connector(connector)) => match result {
                Ok(Ok(channel))=>Ok(channel),
                _=>Err(Status::unavailable("venue authenticated connection failed")),
            },
            _=stop.changed()=>Err(Status::cancelled("venue connect cancelled")),
        };
        let channel = match connection {
            Ok(channel) => channel,
            Err(error) => {
                owner.close().await;
                return Err(error);
            }
        };
        Ok(Self {
            channel: Some(channel),
            tenant: config.tenant,
            owner,
        })
    }
    /// Consumes the owner, closes its channel, aborts and joins all owned transport work.
    pub async fn close(mut self) {
        self.channel.take();
        self.owner.close().await;
    }
}
impl Owner {
    pub(crate) async fn close(&mut self) {
        for task in self.tasks.abort() {
            let _ = task.await;
        }
        if let Some(source) = self.source.take() {
            let _ = source.shutdown_with_timeout(Duration::from_secs(2)).await;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn ipv6_normalization_preserves_endpoint_authority() {
        for (address, expected) in [
            ("https://[::1]:8443", "::1"),
            ("https://[2001:db8::1]", "2001:db8::1"),
            ("https://127.0.0.1", "127.0.0.1"),
            ("https://venue.internal", "venue.internal"),
        ] {
            let uri: http::Uri = address.parse().unwrap();
            let authority = uri.authority().unwrap().clone();
            let host = connection_host(&uri).unwrap();
            assert_eq!(host, expected);
            assert!(rustls::pki_types::ServerName::try_from(host).is_ok());
            assert_eq!(uri.authority().unwrap(), &authority);
        }
    }
    #[tokio::test]
    async fn close_joins_owned_tasks_and_prevents_later_spawn() {
        let tasks = OwnedExecutor::default();
        let (started_tx, started_rx) = tokio::sync::oneshot::channel();
        tasks.execute(Box::pin(async move {
            let _ = started_tx.send(());
            std::future::pending::<()>().await
        }));
        started_rx.await.unwrap();
        let mut owner = Owner {
            tasks: tasks.clone(),
            source: None,
        };
        owner.close().await;
        assert!(tasks.0.lock().unwrap().handles.is_empty());
        let marker = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let copy = marker.clone();
        tasks.execute(Box::pin(async move {
            copy.store(true, std::sync::atomic::Ordering::SeqCst);
        }));
        tokio::task::yield_now().await;
        assert!(!marker.load(std::sync::atomic::Ordering::SeqCst));
    }
}

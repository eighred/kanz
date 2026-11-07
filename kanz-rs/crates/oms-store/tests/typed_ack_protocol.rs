//! Encoding/exchange evidence only: this peer is not PostgreSQL and cannot prove
//! SQL, RLS or transaction correctness. The real recovery corpus remains required.
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::time::Duration;
use tokio_postgres::{types::Type, NoTls};

const SQL: &str = "INSERT INTO execution_recovery_acks (case_id,execution_key,book) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING";

fn body(stream: &mut TcpStream) -> Vec<u8> {
    let mut length = [0; 4];
    stream.read_exact(&mut length).unwrap();
    let length = u32::from_be_bytes(length) as usize;
    assert!((4..=4096).contains(&length));
    let mut bytes = vec![0; length - 4];
    stream.read_exact(&mut bytes).unwrap();
    bytes
}

fn send(stream: &mut TcpStream, kind: u8, bytes: &[u8]) {
    stream.write_all(&[kind]).unwrap();
    stream
        .write_all(&((bytes.len() + 4) as u32).to_be_bytes())
        .unwrap();
    stream.write_all(bytes).unwrap();
}

fn batch(stream: &mut TcpStream) -> Vec<u8> {
    let mut kinds = Vec::new();
    for _ in 0..8 {
        let mut kind = [0];
        stream.read_exact(&mut kind).unwrap();
        let _opaque = body(stream);
        kinds.push(kind[0]);
        if kind[0] == b'S' {
            return kinds;
        }
    }
    panic!("bounded frontend batch missing Sync");
}

async fn witness(typed: bool, affected: u64) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    // Accept is bounded too, including client-connect failure paths.
    listener.set_nonblocking(true).unwrap();
    let peer = std::thread::spawn(move || {
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        let mut stream = loop {
            match listener.accept() {
                Ok((stream, _)) => break stream,
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    assert!(std::time::Instant::now() < deadline);
                    std::thread::sleep(Duration::from_millis(1));
                }
                Err(_) => panic!("loopback accept failed"),
            }
        };
        stream
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        stream
            .set_write_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        let _startup = body(&mut stream);
        send(&mut stream, b'R', &[0, 0, 0, 0]);
        send(&mut stream, b'Z', b'I');
        let first = batch(&mut stream);
        // No response has yet been sent to Parse: this is a causal exchange
        // discriminator, not a scheduler/elapsed-time assertion.
        if typed {
            assert_eq!(first, b"PBDES");
        } else {
            assert_eq!(first, b"PDS");
        }
        send(&mut stream, b'1', &[]);
        if typed {
            send(&mut stream, b'2', &[]);
        }
        send(
            &mut stream,
            b't',
            &[0, 3, 0, 0, 0, 25, 0, 0, 0, 25, 0, 0, 0, 25],
        );
        send(&mut stream, b'n', &[]);
        if !typed {
            send(&mut stream, b'Z', b'I');
            assert_eq!(batch(&mut stream), b"BES");
            send(&mut stream, b'2', &[]);
        }
        let tag = format!("INSERT 0 {affected}\0");
        send(&mut stream, b'C', tag.as_bytes());
        send(&mut stream, b'Z', b'I');
        // Closing this owned socket ends the client driver; no detached work.
    });
    let outcome = async {
        let mut config = tokio_postgres::Config::new();
        config
            .host("127.0.0.1")
            .port(address.port())
            .user("synthetic")
            .ssl_mode(tokio_postgres::config::SslMode::Disable)
            .connect_timeout(Duration::from_secs(5));
        let (client, connection) = match config.connect(NoTls).await {
            Ok(pair) => pair,
            Err(error) => return Ok(Err(error)),
        };
        let driver = tokio::spawn(connection);
        let result = tokio::time::timeout(Duration::from_secs(5), async {
            if typed {
                client
                    .execute_typed(
                        SQL,
                        &[
                            (&"case", Type::TEXT),
                            (&"execution", Type::TEXT),
                            (&"ledger", Type::TEXT),
                        ],
                    )
                    .await
            } else {
                client
                    .execute(SQL, &[&"case", &"execution", &"ledger"])
                    .await
            }
        })
        .await;
        drop(client);
        driver.abort();
        let _closed = driver.await;
        result
    }
    .await;
    // Join even when the client operation refuses or times out.
    peer.join().unwrap();
    assert_eq!(outcome.unwrap().unwrap(), affected);
}

#[tokio::test]
async fn typed_ack_has_no_prepare_response_dependency() {
    for affected in [0, 1] {
        witness(false, affected).await;
        witness(true, affected).await;
    }
}

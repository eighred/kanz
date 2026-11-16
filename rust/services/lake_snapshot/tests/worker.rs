#![cfg(all(target_os = "linux", target_arch = "x86_64"))]

use kanz_lake_snapshot::proto::{
    envelope::v1::{Envelope, EventFrame},
    lake::{v1 as lake, worker::v1 as worker},
};
use kanz_lake_snapshot::{MAX_CONTROL_BYTES, MAX_PARQUET_BYTES, PROTOCOL_VERSION};
use prost::Message;
use sha2::{Digest, Sha256};
use std::io::{Read, Write};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

fn invoke(request: worker::WorkerRequest, body: &[u8]) -> worker::WorkerResponse {
    let encoded = request.encode_to_vec();
    let mut input = (encoded.len() as u32).to_be_bytes().to_vec();
    input.extend(encoded);
    input.extend(body);
    invoke_bytes(input)
}

fn invoke_bytes(input: Vec<u8>) -> worker::WorkerResponse {
    let mut child = Command::new(env!("CARGO_BIN_EXE_kanz-lake-snapshot"))
        .env_clear()
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("spawn exact worker");
    let mut stdin = child.stdin.take().unwrap();
    let writer = std::thread::spawn(move || stdin.write_all(&input));
    let stdout = child.stdout.take().unwrap();
    let reader = std::thread::spawn(move || {
        let mut bytes = Vec::new();
        stdout
            .take((MAX_PARQUET_BYTES + MAX_CONTROL_BYTES + 5) as u64)
            .read_to_end(&mut bytes)
            .unwrap();
        bytes
    });
    let stderr = child.stderr.take().unwrap();
    let errors = std::thread::spawn(move || {
        let mut bytes = Vec::new();
        stderr.take(8193).read_to_end(&mut bytes).unwrap();
        bytes
    });
    let until = Instant::now() + Duration::from_secs(35);
    let status = loop {
        if let Some(status) = child.try_wait().unwrap() {
            break status;
        }
        if Instant::now() >= until {
            child.kill().unwrap();
            child.wait().unwrap();
            writer.join().unwrap().ok();
            reader.join().unwrap();
            errors.join().unwrap();
            panic!("worker exceeded parent observation bound; killed and joined");
        }
        std::thread::sleep(Duration::from_millis(10));
    };
    writer.join().unwrap().expect("complete request input");
    let bytes = reader.join().unwrap();
    let stderr = errors.join().unwrap();
    assert!(
        status.success(),
        "worker refused startup/exited: {status}; stderr bytes={}",
        stderr.len()
    );
    assert!(stderr.is_empty(), "worker emitted unprojected diagnostics");
    assert!(bytes.len() >= 4);
    let length = u32::from_be_bytes(bytes[..4].try_into().unwrap()) as usize;
    assert_eq!(bytes.len(), length + 4, "extra/truncated output");
    let response = worker::WorkerResponse::decode(&bytes[4..]).unwrap();
    assert_eq!(response.protocol_version, PROTOCOL_VERSION);
    response
}

fn row(ordinal: u64, kind: &str) -> worker::CapturedRow {
    let time = prost_types::Timestamp {
        seconds: -1,
        nanos: 999_999_999,
    };
    let mut raw = EventFrame {
        envelope: Some(Envelope {
            tenant_id: "tenant-a".into(),
            event_id: "same-event".into(),
            event_type: kind.into(),
            event_time: Some(time),
            ..Default::default()
        }),
        // Opaque payload deliberately includes a non-UTF8 value and a large
        // integer-looking byte sequence; the worker never parses it as a float.
        payload: vec![
            0xff, 0x00, 0x08, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f,
        ],
    }
    .encode_to_vec();
    raw.extend([0xa0, 0x06, 0x7b]); // Unknown EventFrame field100 remains exact.
    worker::CapturedRow {
        ordinal,
        raw_sha256: Sha256::digest(&raw).to_vec(),
        raw_frame: raw,
        event_id: "same-event".into(),
        event_type: kind.into(),
        event_time: Some(time),
        decode_status: if ordinal == 0 { 3 } else { 1 },
        kafka: None,
    }
}

fn convert(rows: Vec<worker::CapturedRow>) -> worker::Converted {
    let response = invoke(
        worker::WorkerRequest {
            protocol_version: 1,
            operation: Some(worker::worker_request::Operation::Convert(
                worker::ConvertRows {
                    tenant: "tenant-a".into(),
                    rows,
                    schema_version: 1,
                },
            )),
        },
        &[],
    );
    match response.outcome.unwrap() {
        worker::worker_response::Outcome::Converted(converted) => converted,
        other => panic!("conversion did not succeed: {other:?}"),
    }
}

fn control(
    converted: &worker::Converted,
    operation: lake::SnapshotQueryOperation,
    after: Option<u64>,
    page: u32,
) -> worker::QueryControl {
    worker::QueryControl {
        parquet_bytes: converted.parquet.len() as u64,
        parquet_sha256: converted.sha256.clone(),
        expected_tenant: "tenant-a".into(),
        expected_row_count: converted.row_count,
        schema_version: 1,
        query: Some(lake::SnapshotQuery {
            operation: operation as i32,
            after_ordinal: after,
            page_size: page,
        }),
    }
}

fn query(control: worker::QueryControl, body: &[u8]) -> lake::SnapshotQueryResult {
    match invoke(
        worker::WorkerRequest {
            protocol_version: 1,
            operation: Some(worker::worker_request::Operation::Query(control)),
        },
        body,
    )
    .outcome
    .unwrap()
    {
        worker::worker_response::Outcome::Query(result) => result,
        other => panic!("query did not succeed: {other:?}"),
    }
}

#[test]
fn actual_sandbox_convert_preserves_exact_wire_and_unknown_coordinates() {
    use arrow_array::{Array, BinaryArray, Int32Array, Int64Array, StringArray};
    let first = row(0, "market.snapshot.test");
    let mut second = first.clone();
    second.ordinal = 7;
    second.kafka = Some(worker::KafkaCoordinates {
        topic: "market".into(),
        partition: 0,
        offset: 0,
    });
    let converted = convert(vec![first.clone(), second.clone()]);
    assert_eq!(converted.row_count, 2);
    assert_eq!(
        converted.sha256,
        Sha256::digest(&converted.parquet).to_vec()
    );
    let reader = parquet::arrow::arrow_reader::ParquetRecordBatchReaderBuilder::try_new(
        bytes::Bytes::from(converted.parquet),
    )
    .unwrap()
    .build()
    .unwrap();
    let batch = reader.into_iter().next().unwrap().unwrap();
    let raw = batch
        .column(7)
        .as_any()
        .downcast_ref::<BinaryArray>()
        .unwrap();
    assert_eq!(raw.value(0), first.raw_frame);
    assert_eq!(raw.value(1), second.raw_frame);
    let topic = batch
        .column(9)
        .as_any()
        .downcast_ref::<StringArray>()
        .unwrap();
    let partition = batch
        .column(10)
        .as_any()
        .downcast_ref::<Int32Array>()
        .unwrap();
    let offset = batch
        .column(11)
        .as_any()
        .downcast_ref::<Int64Array>()
        .unwrap();
    assert!(topic.is_null(0) && partition.is_null(0) && offset.is_null(0));
    assert_eq!((partition.value(1), offset.value(1)), (0, 0));
    assert!(!partition.is_null(1) && !offset.is_null(1));
}

#[test]
fn actual_datafusion_queries_preserve_zero_cursor_duplicates_and_nanoseconds() {
    let converted = convert(vec![
        row(0, "market.snapshot.a"),
        row(7, "market.snapshot.a"),
        row(9, "market.snapshot.b"),
    ]);
    let first = query(
        control(
            &converted,
            lake::SnapshotQueryOperation::ListEvents,
            None,
            1,
        ),
        &converted.parquet,
    );
    assert_eq!(first.events.len(), 1);
    assert_eq!(first.events[0].ordinal, 0);
    assert_eq!(first.next_ordinal, Some(0));
    assert!(!first.exhausted);
    assert_eq!(
        first.events[0].event_time,
        Some(prost_types::Timestamp {
            seconds: -1,
            nanos: 999_999_999
        })
    );
    let next = query(
        control(
            &converted,
            lake::SnapshotQueryOperation::ListEvents,
            Some(0),
            2,
        ),
        &converted.parquet,
    );
    assert_eq!(
        next.events.iter().map(|e| e.ordinal).collect::<Vec<_>>(),
        [7, 9]
    );
    assert!(next.exhausted && next.next_ordinal.is_none());
    let counts = query(
        control(
            &converted,
            lake::SnapshotQueryOperation::CountByEventType,
            None,
            2,
        ),
        &converted.parquet,
    );
    assert_eq!(
        counts
            .counts
            .iter()
            .map(|c| (c.event_type.as_str(), c.count))
            .collect::<Vec<_>>(),
        [("market.snapshot.a", 2), ("market.snapshot.b", 1)]
    );
    let failures = query(
        control(
            &converted,
            lake::SnapshotQueryOperation::DecodeFailures,
            None,
            5,
        ),
        &converted.parquet,
    );
    assert_eq!(failures.events.len(), 1);
    assert_eq!(failures.events[0].ordinal, 0);
    assert!(failures.exhausted);
}

#[test]
fn actual_worker_refuses_scope_hash_projection_order_and_query_capacity() {
    use worker::{worker_request::Operation, worker_response::Outcome};
    let converted = convert(vec![
        row(0, "market.snapshot.a"),
        row(1, "market.snapshot.b"),
    ]);
    for kind in 0..5 {
        let mut request = control(
            &converted,
            lake::SnapshotQueryOperation::CountByEventType,
            None,
            2,
        );
        match kind {
            0 => request.expected_tenant = "tenant-b".into(),
            1 => request.parquet_sha256[0] ^= 1,
            2 => request.expected_row_count += 1,
            3 => request.query.as_mut().unwrap().after_ordinal = Some(0),
            _ => request.query.as_mut().unwrap().page_size = 1,
        }
        assert!(
            matches!(
                invoke(
                    worker::WorkerRequest {
                        protocol_version: 1,
                        operation: Some(Operation::Query(request))
                    },
                    &converted.parquet
                )
                .outcome,
                Some(Outcome::Failure(_))
            ),
            "mutation {kind}"
        );
    }
    for kind in 0..4 {
        let mut rows = vec![row(0, "market.snapshot.a"), row(1, "market.snapshot.a")];
        match kind {
            0 => rows[0].event_id = "changed".into(),
            1 => rows[0].raw_sha256[0] ^= 1,
            2 => rows[1].ordinal = 0,
            _ => rows[0].event_time.as_mut().unwrap().nanos = 1_000_000_000,
        }
        assert!(
            matches!(
                invoke(
                    worker::WorkerRequest {
                        protocol_version: 1,
                        operation: Some(Operation::Convert(worker::ConvertRows {
                            tenant: "tenant-a".into(),
                            rows,
                            schema_version: 1
                        }))
                    },
                    &[]
                )
                .outcome,
                Some(Outcome::Failure(_))
            ),
            "custody mutation {kind}"
        );
    }
}

#[test]
fn actual_maximum_row_snapshot_crosses_checked_arrow_versions() {
    use kanz_lake_snapshot::MAX_ROWS;
    let rows = (0..MAX_ROWS)
        .map(|i| row(i as u64, "market.snapshot.bound"))
        .collect();
    let converted = convert(rows);
    assert_eq!(converted.row_count, MAX_ROWS as u64);
    let counts = query(
        control(
            &converted,
            lake::SnapshotQueryOperation::CountByEventType,
            None,
            1,
        ),
        &converted.parquet,
    );
    assert_eq!(counts.counts.len(), 1);
    assert_eq!(counts.counts[0].count, MAX_ROWS as u64);
    let tail = query(
        control(
            &converted,
            lake::SnapshotQueryOperation::ListEvents,
            Some((MAX_ROWS - 2) as u64),
            1,
        ),
        &converted.parquet,
    );
    assert_eq!(tail.events[0].ordinal, (MAX_ROWS - 1) as u64);
    assert_eq!(tail.events[0].event_time.unwrap().nanos, 999_999_999);
    assert!(tail.exhausted);
}

#[test]
fn actual_worker_refuses_hash_bound_wrong_storage_type_and_row_tenant() {
    use arrow_array::{Int64Array, RecordBatch, StringArray};
    use arrow_schema::{DataType, Field, Schema};
    use parquet::arrow::{arrow_reader::ParquetRecordBatchReaderBuilder, ArrowWriter};
    use parquet::basic::Compression;
    use parquet::file::{metadata::KeyValue, properties::WriterProperties};
    use std::sync::Arc;
    let original = convert(vec![row(0, "market.snapshot.a")]);
    for mutation in 0..3 {
        let batch =
            ParquetRecordBatchReaderBuilder::try_new(bytes::Bytes::from(original.parquet.clone()))
                .unwrap()
                .build()
                .unwrap()
                .next()
                .unwrap()
                .unwrap();
        let mut fields = batch.schema().fields().to_vec();
        let mut columns = batch.columns().to_vec();
        if mutation == 1 {
            fields[0] = Arc::new(Field::new("ordinal", DataType::Int64, false));
            columns[0] = Arc::new(Int64Array::from(vec![0]));
        } else if mutation == 2 {
            columns[1] = Arc::new(StringArray::from(vec!["tenant-b"]));
        }
        let schema = Arc::new(Schema::new(fields));
        let altered = RecordBatch::try_new(schema.clone(), columns).unwrap();
        let properties = WriterProperties::builder()
            .set_compression(Compression::UNCOMPRESSED)
            .set_max_row_group_row_count(Some(1024))
            .set_key_value_metadata(Some(vec![
                KeyValue::new("kanz.snapshot.schema".to_owned(), "1".to_owned()),
                KeyValue::new("kanz.snapshot.tenant".to_owned(), "tenant-a".to_owned()),
            ]))
            .build();
        let mut body = Vec::new();
        {
            let mut writer = ArrowWriter::try_new(&mut body, schema, Some(properties)).unwrap();
            writer.write(&altered).unwrap();
            writer.close().unwrap();
        }
        let altered = worker::Converted {
            sha256: Sha256::digest(&body).to_vec(),
            parquet: body,
            row_count: 1,
        };
        let response = invoke(
            worker::WorkerRequest {
                protocol_version: 1,
                operation: Some(worker::worker_request::Operation::Query(control(
                    &altered,
                    lake::SnapshotQueryOperation::ListEvents,
                    None,
                    1,
                ))),
            },
            &altered.parquet,
        );
        if mutation == 0 {
            match response.outcome.unwrap() {
                worker::worker_response::Outcome::Query(result) => {
                    assert_eq!(result.events.len(), 1);
                    assert_eq!(result.events[0].ordinal, 0);
                    assert_eq!(result.events[0].event_type, "market.snapshot.a");
                    assert!(result.exhausted);
                }
                other => panic!("unchanged parent rewrite refused: {other:?}"),
            }
        } else {
            assert!(
                matches!(response.outcome, Some(worker::worker_response::Outcome::Failure(failure))
                if failure.code == worker::WorkerFailureCode::InvalidParquet as i32),
                "storage mutation {mutation}"
            );
        }
    }
}

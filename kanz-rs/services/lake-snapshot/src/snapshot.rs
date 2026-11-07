use crate::proto::{envelope::v1::EventFrame, lake::worker::v1 as worker};
use crate::{Failure, MAX_PARQUET_BYTES, MAX_RAW_FRAME, MAX_ROWS};
use bytes::Bytes;
use datafusion::arrow::array::{
    Array, ArrayRef, BinaryArray, Int32Array, Int64Array, StringArray, UInt64Array,
};
use datafusion::arrow::datatypes::{DataType, Field, Schema};
use datafusion::arrow::record_batch::RecordBatch;
use parquet::arrow::{arrow_reader::ParquetRecordBatchReaderBuilder, ArrowWriter};
use parquet::basic::Compression;
use parquet::file::{metadata::KeyValue, properties::WriterProperties};
use prost::Message;
use sha2::{Digest, Sha256};
use std::{io::Write, sync::Arc};

struct CappedOutput(Vec<u8>);
impl Write for CappedOutput {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        if bytes.len() > MAX_PARQUET_BYTES.saturating_sub(self.0.len()) {
            return Err(std::io::Error::other("snapshot capacity"));
        }
        self.0
            .try_reserve(bytes.len())
            .map_err(std::io::Error::other)?;
        self.0.extend_from_slice(bytes);
        Ok(bytes.len())
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

pub fn schema() -> Arc<Schema> {
    Arc::new(Schema::new(vec![
        Field::new("ordinal", DataType::UInt64, false),
        Field::new("tenant", DataType::Utf8, false),
        Field::new("event_id", DataType::Utf8, false),
        Field::new("event_type", DataType::Utf8, false),
        Field::new("event_seconds", DataType::Int64, false),
        Field::new("event_nanos", DataType::Int32, false),
        Field::new("decode_status", DataType::Int32, false),
        Field::new("raw_frame", DataType::Binary, false),
        Field::new("raw_sha256", DataType::Binary, false),
        Field::new("kafka_topic", DataType::Utf8, true),
        Field::new("kafka_partition", DataType::Int32, true),
        Field::new("kafka_offset", DataType::Int64, true),
    ]))
}

fn stored_schema(tenant: &str) -> Schema {
    Schema::new_with_metadata(
        schema().fields().clone(),
        std::collections::HashMap::from([
            ("kanz.snapshot.schema".to_owned(), "1".to_owned()),
            ("kanz.snapshot.tenant".to_owned(), tenant.to_owned()),
        ]),
    )
}

pub fn valid_text(text: &str) -> bool {
    !text.is_empty() && text.len() <= 256 && !text.chars().any(char::is_control)
}

pub fn valid_time(time: &prost_types::Timestamp) -> bool {
    (-62_135_596_800..=253_402_300_799).contains(&time.seconds)
        && (0..1_000_000_000).contains(&time.nanos)
}

fn valid_row(row: &worker::CapturedRow, tenant: &str) -> Result<(), Failure> {
    if !valid_text(&row.event_id)
        || !valid_text(&row.event_type)
        || row.raw_frame.is_empty()
        || row.raw_frame.len() > MAX_RAW_FRAME
        || row.raw_sha256.as_slice() != Sha256::digest(&row.raw_frame).as_slice()
        || !(1..=3).contains(&row.decode_status)
    {
        return Err(Failure::InvalidInput);
    }
    let time = row.event_time.as_ref().ok_or(Failure::InvalidInput)?;
    if !valid_time(time) {
        return Err(Failure::InvalidInput);
    }
    if let Some(kafka) = &row.kafka {
        if !valid_text(&kafka.topic) || kafka.partition < 0 || kafka.offset < 0 {
            return Err(Failure::InvalidInput);
        }
    }
    // Go owns full canonical bus validation. Independently bind the projected
    // identity/time and tenant to these original bytes; never re-encode them.
    let frame = EventFrame::decode(row.raw_frame.as_slice()).map_err(|_| Failure::InvalidInput)?;
    let envelope = frame.envelope.ok_or(Failure::InvalidInput)?;
    if envelope.tenant_id != tenant
        || envelope.event_id != row.event_id
        || envelope.event_type != row.event_type
        || envelope.event_time.as_ref() != Some(time)
    {
        return Err(Failure::InvalidInput);
    }
    Ok(())
}

pub fn convert(input: worker::ConvertRows) -> Result<worker::Converted, Failure> {
    if input.schema_version != 1 || !valid_text(&input.tenant) || input.rows.len() > MAX_ROWS {
        return Err(Failure::InvalidInput);
    }
    let mut previous = None;
    for row in &input.rows {
        if previous.is_some_and(|ordinal| row.ordinal <= ordinal) {
            return Err(Failure::InvalidInput);
        }
        valid_row(row, &input.tenant)?;
        previous = Some(row.ordinal);
    }
    let columns: Vec<ArrayRef> = vec![
        Arc::new(UInt64Array::from_iter_values(
            input.rows.iter().map(|r| r.ordinal),
        )),
        Arc::new(StringArray::from_iter_values(
            input.rows.iter().map(|_| input.tenant.as_str()),
        )),
        Arc::new(StringArray::from_iter_values(
            input.rows.iter().map(|r| r.event_id.as_str()),
        )),
        Arc::new(StringArray::from_iter_values(
            input.rows.iter().map(|r| r.event_type.as_str()),
        )),
        Arc::new(Int64Array::from_iter_values(
            input
                .rows
                .iter()
                .map(|r| r.event_time.as_ref().expect("validated time").seconds),
        )),
        Arc::new(Int32Array::from_iter_values(
            input
                .rows
                .iter()
                .map(|r| r.event_time.as_ref().expect("validated time").nanos),
        )),
        Arc::new(Int32Array::from_iter_values(
            input.rows.iter().map(|r| r.decode_status),
        )),
        Arc::new(BinaryArray::from_iter_values(
            input.rows.iter().map(|r| r.raw_frame.as_slice()),
        )),
        Arc::new(BinaryArray::from_iter_values(
            input.rows.iter().map(|r| r.raw_sha256.as_slice()),
        )),
        Arc::new(StringArray::from(
            input
                .rows
                .iter()
                .map(|r| r.kafka.as_ref().map(|k| k.topic.as_str()))
                .collect::<Vec<_>>(),
        )),
        Arc::new(Int32Array::from(
            input
                .rows
                .iter()
                .map(|r| r.kafka.as_ref().map(|k| k.partition))
                .collect::<Vec<_>>(),
        )),
        Arc::new(Int64Array::from(
            input
                .rows
                .iter()
                .map(|r| r.kafka.as_ref().map(|k| k.offset))
                .collect::<Vec<_>>(),
        )),
    ];
    let rows = input.rows.len() as u64;
    let tenant = input.tenant.clone();
    drop(input);
    let batch = RecordBatch::try_new(schema(), columns).map_err(|_| Failure::InvalidInput)?;
    let properties = WriterProperties::builder()
        .set_compression(Compression::UNCOMPRESSED)
        .set_max_row_group_row_count(Some(1024))
        .set_created_by("kanz-lake-snapshot/v1".to_owned())
        .set_key_value_metadata(Some(vec![
            KeyValue::new("kanz.snapshot.schema".to_owned(), "1".to_owned()),
            KeyValue::new("kanz.snapshot.tenant".to_owned(), tenant),
        ]))
        .build();
    let mut output = CappedOutput(Vec::new());
    {
        let mut writer = ArrowWriter::try_new(&mut output, schema(), Some(properties))
            .map_err(|_| Failure::InvalidParquet)?;
        writer.write(&batch).map_err(|_| Failure::InvalidParquet)?;
        writer.close().map_err(|_| Failure::InvalidParquet)?;
    }
    let parquet = output.0;
    if parquet.len() > MAX_PARQUET_BYTES {
        return Err(Failure::Capacity);
    }
    let sha256 = Sha256::digest(&parquet).to_vec();
    Ok(worker::Converted {
        parquet,
        row_count: rows,
        sha256,
    })
}

/// Validate metadata before record-array decoding, then recheck every row.
/// Footer/schema parsing itself allocates: the OS address-space limit remains
/// necessary alongside the encoded-file, decoded-array and query-pool limits.
pub fn read_parquet(
    control: &worker::QueryControl,
    body: Vec<u8>,
) -> Result<Vec<RecordBatch>, Failure> {
    if control.schema_version != 1
        || !valid_text(&control.expected_tenant)
        || control.expected_row_count > MAX_ROWS as u64
        || body.len() > MAX_PARQUET_BYTES
        || body.len() as u64 != control.parquet_bytes
        || control.parquet_sha256.as_slice() != Sha256::digest(&body).as_slice()
    {
        return Err(Failure::InvalidParquet);
    }
    let builder = ParquetRecordBatchReaderBuilder::try_new(Bytes::from(body))
        .map_err(|_| Failure::InvalidParquet)?;
    if builder.schema().as_ref() != &stored_schema(&control.expected_tenant) {
        return Err(Failure::InvalidParquet);
    }
    let metadata = builder.metadata();
    if metadata.file_metadata().num_rows() != control.expected_row_count as i64
        || metadata.num_row_groups() > 100
    {
        return Err(Failure::Capacity);
    }
    let values = metadata
        .file_metadata()
        .key_value_metadata()
        .ok_or(Failure::InvalidParquet)?;
    if values.len() != 3
        || values.iter().any(|value| {
            !matches!(
                value.key.as_str(),
                "kanz.snapshot.schema" | "kanz.snapshot.tenant" | "ARROW:schema"
            )
        })
        || values
            .iter()
            .filter(|value| value.key == "ARROW:schema" && value.value.is_some())
            .count()
            != 1
    {
        return Err(Failure::InvalidParquet);
    }
    for (name, expected) in [
        ("kanz.snapshot.schema", "1"),
        ("kanz.snapshot.tenant", control.expected_tenant.as_str()),
    ] {
        let found: Vec<_> = values.iter().filter(|v| v.key == name).collect();
        if found.len() != 1 || found[0].value.as_deref() != Some(expected) {
            return Err(Failure::InvalidParquet);
        }
    }
    let mut uncompressed = 0u64;
    for group in metadata.row_groups() {
        if group.num_rows() < 0 || group.num_rows() > 1024 {
            return Err(Failure::Capacity);
        }
        for column in group.columns() {
            if column.file_path().is_some()
                || column.compression() != Compression::UNCOMPRESSED
                || column.uncompressed_size() < 0
                || column.compressed_size() < 0
            {
                return Err(Failure::InvalidParquet);
            }
            uncompressed = uncompressed
                .checked_add(column.uncompressed_size() as u64)
                .ok_or(Failure::Capacity)?;
            if uncompressed > MAX_PARQUET_BYTES as u64 {
                return Err(Failure::Capacity);
            }
        }
    }
    let reader = builder
        .with_batch_size(256)
        .build()
        .map_err(|_| Failure::InvalidParquet)?;
    let mut batches = Vec::new();
    let mut previous = None;
    let mut rows = 0u64;
    let mut memory = 0usize;
    for batch in reader {
        let batch = batch.map_err(|_| Failure::InvalidParquet)?;
        memory = memory
            .checked_add(batch.get_array_memory_size())
            .ok_or(Failure::Capacity)?;
        rows = rows
            .checked_add(batch.num_rows() as u64)
            .ok_or(Failure::Capacity)?;
        if memory > MAX_PARQUET_BYTES || rows > control.expected_row_count {
            return Err(Failure::Capacity);
        }
        for index in 0..batch.num_rows() {
            let row = captured_row(&batch, index)?;
            let tenant = batch
                .column(1)
                .as_any()
                .downcast_ref::<StringArray>()
                .ok_or(Failure::InvalidParquet)?;
            if tenant.is_null(index)
                || tenant.value(index) != control.expected_tenant
                || previous.is_some_and(|ordinal| row.ordinal <= ordinal)
            {
                return Err(Failure::InvalidParquet);
            }
            valid_row(&row, &control.expected_tenant)?;
            previous = Some(row.ordinal);
        }
        // The exact tenant-bound stored metadata was checked above. Rebind
        // only that known schema metadata for the private in-memory table;
        // all arrays, field definitions and original bytes remain unchanged.
        batches.push(
            RecordBatch::try_new(schema(), batch.columns().to_vec())
                .map_err(|_| Failure::InvalidParquet)?,
        );
    }
    if rows != control.expected_row_count {
        return Err(Failure::InvalidParquet);
    }
    Ok(batches)
}

fn captured_row(batch: &RecordBatch, row: usize) -> Result<worker::CapturedRow, Failure> {
    macro_rules! value {
        ($index:expr, $kind:ty) => {{
            let array = batch
                .column($index)
                .as_any()
                .downcast_ref::<$kind>()
                .ok_or(Failure::InvalidParquet)?;
            if array.is_null(row) {
                return Err(Failure::InvalidParquet);
            }
            array.value(row)
        }};
    }
    let topic = batch
        .column(9)
        .as_any()
        .downcast_ref::<StringArray>()
        .ok_or(Failure::InvalidParquet)?;
    let partition = batch
        .column(10)
        .as_any()
        .downcast_ref::<Int32Array>()
        .ok_or(Failure::InvalidParquet)?;
    let offset = batch
        .column(11)
        .as_any()
        .downcast_ref::<Int64Array>()
        .ok_or(Failure::InvalidParquet)?;
    let presence = [
        !topic.is_null(row),
        !partition.is_null(row),
        !offset.is_null(row),
    ];
    if presence.iter().any(|v| *v != presence[0]) {
        return Err(Failure::InvalidParquet);
    }
    if presence[0]
        && (!valid_text(topic.value(row)) || partition.value(row) < 0 || offset.value(row) < 0)
    {
        return Err(Failure::InvalidParquet);
    }
    let kafka = presence[0].then(|| worker::KafkaCoordinates {
        topic: topic.value(row).to_owned(),
        partition: partition.value(row),
        offset: offset.value(row),
    });
    let id = value!(2, StringArray);
    let event_type = value!(3, StringArray);
    let raw = value!(7, BinaryArray);
    let digest = value!(8, BinaryArray);
    if !valid_text(id) || !valid_text(event_type) || raw.len() > MAX_RAW_FRAME || digest.len() != 32
    {
        return Err(Failure::InvalidParquet);
    }
    Ok(worker::CapturedRow {
        ordinal: value!(0, UInt64Array),
        event_id: id.to_owned(),
        event_type: event_type.to_owned(),
        event_time: Some(prost_types::Timestamp {
            seconds: value!(4, Int64Array),
            nanos: value!(5, Int32Array),
        }),
        decode_status: value!(6, Int32Array),
        raw_frame: raw.to_vec(),
        raw_sha256: digest.to_vec(),
        kafka,
    })
}

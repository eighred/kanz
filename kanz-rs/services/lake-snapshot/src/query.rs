use crate::proto::lake::{v1 as lake, worker::v1 as worker};
use crate::{snapshot, Failure, MAX_PAGE, MAX_QUERY_BYTES};
use datafusion::arrow::array::{Array, Int32Array, Int64Array, StringArray, UInt64Array};
use datafusion::execution::{
    disk_manager::{DiskManagerBuilder, DiskManagerMode},
    memory_pool::GreedyMemoryPool,
    runtime_env::RuntimeEnvBuilder,
};
use datafusion::functions_aggregate::count::count;
use datafusion::prelude::{col, lit, SessionConfig, SessionContext};
use prost::Message;
use std::sync::Arc;

pub async fn execute(
    control: worker::QueryControl,
    body: Vec<u8>,
) -> Result<lake::SnapshotQueryResult, Failure> {
    let query = control.query.as_ref().ok_or(Failure::InvalidInput)?;
    let operation = lake::SnapshotQueryOperation::try_from(query.operation)
        .map_err(|_| Failure::InvalidInput)?;
    let page = query.page_size as usize;
    if page == 0
        || page > MAX_PAGE
        || operation == lake::SnapshotQueryOperation::Unspecified
        || (operation == lake::SnapshotQueryOperation::CountByEventType
            && query.after_ordinal.is_some())
    {
        return Err(Failure::InvalidInput);
    }
    let batches = snapshot::read_parquet(&control, body)?;
    let runtime = RuntimeEnvBuilder::new()
        .with_memory_pool(Arc::new(GreedyMemoryPool::new(128 * 1024 * 1024)))
        .with_disk_manager_builder(
            DiskManagerBuilder::default().with_mode(DiskManagerMode::Disabled),
        )
        .build_arc()
        .map_err(|_| Failure::Query)?;
    let context = SessionContext::new_with_config_rt(
        SessionConfig::new()
            .with_target_partitions(1)
            .with_batch_size(256),
        runtime,
    );
    // This table is constructed only from validated in-memory batches. SQL and
    // Parquet/object-store table registration are absent from this interface.
    let table = datafusion::datasource::MemTable::try_new(snapshot::query_schema(), vec![batches])
        .map_err(|_| Failure::Query)?;
    let mut frame = context
        .read_table(Arc::new(table))
        .map_err(|_| Failure::Query)?;
    if let Some(after) = query.after_ordinal {
        frame = frame
            .filter(col("ordinal").gt(lit(after)))
            .map_err(|_| Failure::Query)?;
    }
    let counts = operation == lake::SnapshotQueryOperation::CountByEventType;
    if counts {
        frame = frame
            .aggregate(
                vec![col("event_type")],
                vec![count(lit(1i64)).alias("count")],
            )
            .map_err(|_| Failure::Query)?
            .sort(vec![col("event_type").sort(true, true)])
            .map_err(|_| Failure::Query)?;
    } else {
        if operation == lake::SnapshotQueryOperation::DecodeFailures {
            frame = frame
                .filter(col("decode_status").eq(lit(3i32)))
                .map_err(|_| Failure::Query)?;
        }
        frame = frame
            .select_columns(&[
                "ordinal",
                "event_id",
                "event_type",
                "event_seconds",
                "event_nanos",
                "decode_status",
            ])
            .map_err(|_| Failure::Query)?
            .sort(vec![col("ordinal").sort(true, true)])
            .map_err(|_| Failure::Query)?;
    }
    let output = frame
        .limit(0, Some(page + 1))
        .map_err(|_| Failure::Query)?
        .collect()
        .await
        .map_err(|_| Failure::Query)?;
    let mut result = lake::SnapshotQueryResult {
        exhausted: true,
        ..Default::default()
    };
    for batch in output {
        for row in 0..batch.num_rows() {
            macro_rules! value {
                ($index:expr, $kind:ty) => {{
                    let array = batch
                        .column($index)
                        .as_any()
                        .downcast_ref::<$kind>()
                        .ok_or(Failure::Query)?;
                    if array.is_null(row) {
                        return Err(Failure::Query);
                    }
                    array.value(row)
                }};
            }
            if counts {
                if result.counts.len() == page {
                    return Err(Failure::Capacity);
                }
                // DataFusion COUNT is Int64, then explicitly checked rather
                // than reinterpreting a negative value as an unsigned count.
                let count = u64::try_from(value!(1, Int64Array)).map_err(|_| Failure::Query)?;
                result.counts.push(lake::SnapshotEventCount {
                    event_type: value!(0, StringArray).to_owned(),
                    count,
                });
            } else if result.events.len() == page {
                result.exhausted = false;
                result.next_ordinal = result.events.last().map(|event| event.ordinal);
            } else {
                result.events.push(lake::SnapshotEvent {
                    ordinal: value!(0, UInt64Array),
                    event_id: value!(1, StringArray).to_owned(),
                    event_type: value!(2, StringArray).to_owned(),
                    event_time: Some(prost_types::Timestamp {
                        seconds: value!(3, Int64Array),
                        nanos: value!(4, Int32Array),
                    }),
                    decode_status: value!(5, Int32Array),
                });
            }
        }
    }
    if result.encoded_len() > MAX_QUERY_BYTES - 64 {
        return Err(Failure::Capacity);
    }
    Ok(result)
}

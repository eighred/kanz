mod sandbox;

use kanz_lake_snapshot::proto::lake::worker::v1::{
    self as worker, worker_request::Operation, worker_response::Outcome,
};
use kanz_lake_snapshot::{
    Failure, MAX_CONTROL_BYTES, MAX_CONVERT_BYTES, MAX_PARQUET_BYTES, MAX_QUERY_BYTES,
    PROTOCOL_VERSION,
};
use prost::Message;

fn main() {
    if std::env::args_os().len() != 1 || !close_inherited_descriptors() {
        std::process::exit(2);
    }
    // Runtime initialization is trusted startup. No timers, network driver or
    // background thread pool is enabled; the installed filter prohibits clone.
    let runtime = match tokio::runtime::Builder::new_current_thread().build() {
        Ok(runtime) => runtime,
        Err(_) => std::process::exit(2),
    };
    if sandbox::install().is_err() {
        std::process::exit(2);
    }
    let result = run(&runtime);
    let outcome = match result {
        Ok(outcome) => outcome,
        Err(failure) => Outcome::Failure(worker::WorkerFailure {
            code: match failure {
                Failure::InvalidInput => worker::WorkerFailureCode::InvalidInput as i32,
                Failure::Capacity => worker::WorkerFailureCode::Capacity as i32,
                Failure::InvalidParquet => worker::WorkerFailureCode::InvalidParquet as i32,
                Failure::Query => worker::WorkerFailureCode::Query as i32,
            },
        }),
    };
    let limit = if matches!(&outcome, Outcome::Converted(_)) {
        MAX_PARQUET_BYTES + MAX_CONTROL_BYTES
    } else {
        MAX_QUERY_BYTES
    };
    let response = worker::WorkerResponse {
        protocol_version: PROTOCOL_VERSION,
        outcome: Some(outcome),
    };
    if response.encoded_len() > limit {
        std::process::exit(1);
    }
    let body = response.encode_to_vec();
    if kanz_lake_snapshot::write_frame(&mut std::io::stdout().lock(), &body, limit).is_err() {
        std::process::exit(1);
    }
}

fn close_inherited_descriptors() -> bool {
    #[cfg(all(target_os = "linux", target_arch = "x86_64"))]
    {
        // The Go owner executes its hash-verified file through inherited fd3.
        // Kernel loading has finished; remove that handle and every other
        // non-stdio descriptor before runtime setup or request processing.
        // SAFETY: fixed scalar arguments; no pointers or borrowed resources.
        unsafe { libc::syscall(libc::SYS_close_range, 3_u32, u32::MAX, 0_u32) == 0 }
    }
    #[cfg(not(all(target_os = "linux", target_arch = "x86_64")))]
    {
        false
    }
}

fn run(runtime: &tokio::runtime::Runtime) -> Result<Outcome, Failure> {
    let mut input = std::io::stdin().lock();
    let encoded = kanz_lake_snapshot::read_frame(&mut input, MAX_CONVERT_BYTES)?;
    let length = encoded.len();
    let request =
        worker::WorkerRequest::decode(encoded.as_slice()).map_err(|_| Failure::InvalidInput)?;
    // The private Go producer emits a canonical message. Reject additional
    // fields/arms and noncanonical control bytes; raw EventFrames remain opaque.
    if request.protocol_version != PROTOCOL_VERSION || request.encode_to_vec() != encoded {
        return Err(Failure::InvalidInput);
    }
    drop(encoded);
    match request.operation.ok_or(Failure::InvalidInput)? {
        Operation::Convert(rows) => {
            kanz_lake_snapshot::require_eof(&mut input)?;
            kanz_lake_snapshot::snapshot::convert(rows).map(Outcome::Converted)
        }
        Operation::Query(control) => {
            if length > MAX_CONTROL_BYTES {
                return Err(Failure::Capacity);
            }
            let size = usize::try_from(control.parquet_bytes).map_err(|_| Failure::Capacity)?;
            let body = kanz_lake_snapshot::read_body(&mut input, size, MAX_PARQUET_BYTES)?;
            kanz_lake_snapshot::require_eof(&mut input)?;
            runtime
                .block_on(kanz_lake_snapshot::query::execute(control, body))
                .map(Outcome::Query)
        }
    }
}

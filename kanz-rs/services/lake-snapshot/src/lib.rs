//! Finite, non-authoritative snapshot computation. Identity and file ownership
//! remain in the Go lake owner; this worker accepts neither paths nor SQL.
use std::io::{Read, Write};

pub mod proto;
pub mod query;
pub mod snapshot;

pub const PROTOCOL_VERSION: u32 = 1;
pub const MAX_CONVERT_BYTES: usize = 64 * 1024 * 1024;
pub const MAX_CONTROL_BYTES: usize = 64 * 1024;
pub const MAX_PARQUET_BYTES: usize = 96 * 1024 * 1024;
pub const MAX_QUERY_BYTES: usize = 1024 * 1024;
pub const MAX_ROWS: usize = 100_000;
pub const MAX_PAGE: usize = 1_000;
pub const MAX_RAW_FRAME: usize = 16 * 1024 * 1024;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Failure {
    InvalidInput,
    Capacity,
    InvalidParquet,
    Query,
}

/// Refuse an advertised size before reading or reserving its body. The caller
/// separately checks the smaller query-control cap after decoding the variant.
pub fn read_frame(input: &mut impl Read, limit: usize) -> Result<Vec<u8>, Failure> {
    let mut prefix = [0; 4];
    input
        .read_exact(&mut prefix)
        .map_err(|_| Failure::InvalidInput)?;
    let length = u32::from_be_bytes(prefix) as usize;
    if length == 0 || length > limit {
        return Err(Failure::Capacity);
    }
    read_body(input, length, limit)
}

pub fn read_body(input: &mut impl Read, length: usize, limit: usize) -> Result<Vec<u8>, Failure> {
    if length > limit {
        return Err(Failure::Capacity);
    }
    let mut body = Vec::new();
    body.try_reserve_exact(length)
        .map_err(|_| Failure::Capacity)?;
    body.resize(length, 0);
    input
        .read_exact(&mut body)
        .map_err(|_| Failure::InvalidInput)?;
    Ok(body)
}

pub fn require_eof(input: &mut impl Read) -> Result<(), Failure> {
    let mut byte = [0];
    match input.read(&mut byte) {
        Ok(0) => Ok(()),
        _ => Err(Failure::InvalidInput),
    }
}

pub fn write_frame(output: &mut impl Write, body: &[u8], limit: usize) -> Result<(), Failure> {
    if body.len() > limit || body.len() > u32::MAX as usize {
        return Err(Failure::Capacity);
    }
    output
        .write_all(&(body.len() as u32).to_be_bytes())
        .map_err(|_| Failure::Query)?;
    output.write_all(body).map_err(|_| Failure::Query)?;
    output.flush().map_err(|_| Failure::Query)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Cursor;

    #[test]
    fn announced_capacity_refuses_before_body_read() {
        let mut input = Cursor::new([0, 0, 1, 0, 42]);
        assert_eq!(read_frame(&mut input, 255), Err(Failure::Capacity));
        assert_eq!(input.position(), 4);
    }

    #[test]
    fn truncated_and_extra_input_are_not_success() {
        assert_eq!(
            read_frame(&mut Cursor::new([0, 0, 0, 2, 1]), 2),
            Err(Failure::InvalidInput)
        );
        assert_eq!(
            require_eof(&mut Cursor::new([0])),
            Err(Failure::InvalidInput)
        );
        assert_eq!(require_eof(&mut Cursor::new([])), Ok(()));
    }
}

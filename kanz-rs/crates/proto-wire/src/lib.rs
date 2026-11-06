#![forbid(unsafe_code)]
//! Go protobuf normalization: known fields are decoded/merged; unknown value
//! bytes retain their original ordering and representation. No financial model.
use prost::Message;
use prost_reflect::{DynamicMessage, Kind, MessageDescriptor};
use std::collections::BTreeMap;
mod canonical;
pub use canonical::{Canonical, Schema};

#[derive(Default)]
struct Unknown {
    raw: Vec<u8>,
    children: BTreeMap<u32, Vec<Unknown>>,
}
impl Unknown {
    fn merge(&mut self, other: Self, desc: &MessageDescriptor, limit: usize) -> Result<(), Error> {
        self.raw.extend(other.raw);
        for (number, mut children) in other.children {
            let field = desc.get_field(number).ok_or(Error::InvalidInput)?;
            let entry = self.children.entry(number).or_default();
            if field.is_list() || entry.is_empty() {
                if limit > MAX_BYTES && entry.len().checked_add(children.len()).is_none_or(|n| n > 8000) { return Err(Error::Capacity); }
                entry.append(&mut children)
            } else if let Kind::Message(child) = field.kind() {
                entry[0].merge(children.remove(0), &child, limit)?
            } else {
                return Err(Error::InvalidInput);
            }
        }
        Ok(())
    }
}
/// Closed budgets: normal consumers remain1MiB; recovery history is the existing4MiB contract.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Budget { Standard, RecoveryHistory }
impl Budget {
    pub fn bytes(self) -> usize { match self { Self::Standard => MAX_BYTES, Self::RecoveryHistory => 4 * MAX_BYTES } }
}
pub fn normalize(desc: MessageDescriptor, raw: &[u8]) -> Result<Vec<u8>, Error> {
    normalize_with_budget(desc, raw, Budget::Standard)
}
pub fn normalize_with_budget(desc: MessageDescriptor, raw: &[u8], budget: Budget) -> Result<Vec<u8>, Error> {
    let limit = budget.bytes();
    if raw.len() > limit { return Err(Error::Capacity); }
    let (known, unknown) = split(&desc, raw, 0, limit)?;
    let value = DynamicMessage::decode(desc.clone(), known.as_slice()).map_err(|_| Error::InvalidInput)?;
    if value.encoded_len() > limit { return Err(Error::Capacity); }
    let encoded = value.encode_to_vec();
    inject(&desc, &encoded, &unknown, 0, limit)
}
fn bounded_append(out: &mut Vec<u8>, raw: &[u8], limit: usize) -> Result<(), Error> {
    if out.len().checked_add(raw.len()).is_none_or(|n| n > limit) { return Err(Error::Capacity); }
    out.extend_from_slice(raw); Ok(())
}
fn bounded_message(n: u32, raw: &[u8], out: &mut Vec<u8>, limit: usize) -> Result<(), Error> {
    fn width(v: u64) -> usize { (64 - v.leading_zeros() as usize).max(1).div_ceil(7) }
    let size = width((u64::from(n) << 3) | 2) + width(raw.len() as u64) + raw.len();
    if out.len().checked_add(size).is_none_or(|n| n > limit) { return Err(Error::Capacity); }
    put_message(n, raw, out); Ok(())
}
fn wire(kind: &Kind) -> u8 {
    match kind {
        Kind::Double | Kind::Fixed64 | Kind::Sfixed64 => 1,
        Kind::Float | Kind::Fixed32 | Kind::Sfixed32 => 5,
        Kind::String | Kind::Bytes | Kind::Message(_) => 2,
        _ => 0,
    }
}
/// Protobuf-known wire types; other encodings with this number remain unknown.
pub fn accepts_wire(field: &prost_reflect::FieldDescriptor, w: u8) -> bool {
    w == wire(&field.kind()) || (field.is_list() && w == 2 && wire(&field.kind()) != 2)
}
fn split(desc: &MessageDescriptor, raw: &[u8], depth: usize, limit: usize) -> Result<(Vec<u8>, Unknown), Error> {
    if depth > 32 {
        return Err(Error::Capacity);
    }
    let mut known = Vec::new();
    let mut unknown = Unknown::default();
    let mut pos = 0;
    while pos < raw.len() {
        let start = pos;
        let (number, w) = key(raw, &mut pos)?;
        let value = pos;
        let field = desc.get_field(number);
        let supported = field.as_ref().is_some_and(|f| accepts_wire(f, w));
        if supported {
            let field = field.ok_or(Error::InvalidInput)?;
            // Current OMS FACT graphs have neither maps nor oneofs. Refuse a
            // future extension until its overwrite/unknown semantics is qualified.
            if field.is_map() || field.containing_oneof().is_some() {
                return Err(Error::InvalidInput);
            }
            if let Kind::Message(child) = field.kind() {
                let bytes = payload(raw, &mut pos)?;
                let (clean, sidecar) = split(&child, bytes, depth + 1, limit)?;
                bounded_message(number, &clean, &mut known, limit)?;
                let entries = unknown.children.entry(number).or_default();
                if field.is_list() || entries.is_empty() {
                    if limit > MAX_BYTES && entries.len() >= 8000 { return Err(Error::Capacity); }
                    entries.push(sidecar)
                } else {
                    entries[0].merge(sidecar, &child, limit)?
                }
            } else {
                skip(raw, &mut pos, number, w)?;
                bounded_append(&mut known, &raw[start..pos], limit)?;
            }
        } else {
            skip(raw, &mut pos, number, w)?;
            // Go consumes the tag varint, then re-encodes that tag canonically;
            // the value bytes (including noncanonical varints) remain untouched.
            put_varint((u64::from(number) << 3) | u64::from(w), &mut unknown.raw);
            unknown.raw.extend_from_slice(&raw[value..pos]);
        }
    }
    Ok((known, unknown))
}
fn inject(
    desc: &MessageDescriptor,
    raw: &[u8],
    unknown: &Unknown,
    depth: usize,
    limit: usize,
) -> Result<Vec<u8>, Error> {
    if depth > 32 {
        return Err(Error::Capacity);
    }
    let mut result = Vec::new();
    let mut pos = 0;
    let mut index = BTreeMap::<u32, usize>::new();
    while pos < raw.len() {
        let start = pos;
        let (number, w) = key(raw, &mut pos)?;
        if let Some(field) = desc.get_field(number) {
            if let Kind::Message(child) = field.kind() {
                if w != 2 {
                    return Err(Error::InvalidInput);
                }
                let bytes = payload(raw, &mut pos)?;
                let i = index.entry(number).or_default();
                let empty = Unknown::default();
                let sidecar = unknown
                    .children
                    .get(&number)
                    .and_then(|v| v.get(*i))
                    .unwrap_or(&empty);
                bounded_message(
                    number,
                    &inject(&child, bytes, sidecar, depth + 1, limit)?,
                    &mut result,
                    limit,
                )?;
                *i += 1;
                continue;
            }
        }
        skip(raw, &mut pos, number, w)?;
        bounded_append(&mut result, &raw[start..pos], limit)?;
    }
    bounded_append(&mut result, &unknown.raw, limit)?;
    Ok(result)
}

pub const MAX_BYTES: usize = 1024 * 1024;
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    InvalidInput,
    Capacity,
}
fn varint(raw: &[u8], pos: &mut usize) -> Result<u64, Error> {
    let mut value = 0u64;
    for shift in (0..70).step_by(7) {
        let b = *raw.get(*pos).ok_or(Error::InvalidInput)?;
        *pos += 1;
        if shift == 63 && b > 1 {
            return Err(Error::InvalidInput);
        }
        value |= u64::from(b & 127) << shift;
        if b < 128 {
            return Ok(value);
        }
    }
    Err(Error::InvalidInput)
}
pub fn key(raw: &[u8], pos: &mut usize) -> Result<(u32, u8), Error> {
    let k = varint(raw, pos)?;
    let n = u32::try_from(k >> 3).map_err(|_| Error::InvalidInput)?;
    if n == 0 || n > 0x1fffffff {
        return Err(Error::InvalidInput);
    }
    Ok((n, (k & 7) as u8))
}
fn slice<'a>(raw: &'a [u8], pos: &mut usize, n: usize) -> Result<&'a [u8], Error> {
    let end = pos.checked_add(n).ok_or(Error::InvalidInput)?;
    let part = raw.get(*pos..end).ok_or(Error::InvalidInput)?;
    *pos = end;
    Ok(part)
}
pub fn payload<'a>(raw: &'a [u8], pos: &mut usize) -> Result<&'a [u8], Error> {
    let n = usize::try_from(varint(raw, pos)?).map_err(|_| Error::InvalidInput)?;
    slice(raw, pos, n)
}
pub fn skip(raw: &[u8], pos: &mut usize, n: u32, wire: u8) -> Result<(), Error> {
    // Unknown groups are scanned iteratively; their complete original wire is retained.
    let mut groups = Vec::new();
    let mut current = (n, wire);
    loop {
        match current.1 {
            0 => {
                varint(raw, pos)?;
            }
            1 => {
                slice(raw, pos, 8)?;
            }
            2 => {
                payload(raw, pos)?;
            }
            3 => {
                if groups.len() >= 10001 {
                    return Err(Error::InvalidInput);
                }
                groups.push(current.0)
            }
            4 => {
                if groups.pop() != Some(current.0) {
                    return Err(Error::InvalidInput);
                }
            }
            5 => {
                slice(raw, pos, 4)?;
            }
            _ => return Err(Error::InvalidInput),
        }
        if groups.is_empty() {
            return Ok(());
        }
        current = key(raw, pos)?;
    }
}
pub fn put_varint(mut v: u64, out: &mut Vec<u8>) {
    while v >= 128 {
        out.push((v as u8 & 127) | 128);
        v >>= 7;
    }
    out.push(v as u8);
}
pub fn put_message(n: u32, raw: &[u8], out: &mut Vec<u8>) {
    put_varint((u64::from(n) << 3) | 2, out);
    put_varint(raw.len() as u64, out);
    out.extend(raw);
}

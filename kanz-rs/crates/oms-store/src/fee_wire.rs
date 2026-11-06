//! Go protobuf equality and deterministic journal representation for canonical Fill.
//! Known fields use generated types; descriptors identify raw unknown fields.
use super::{Error, MAX_BYTES};
use kanz_oms_fold_shadow::proto::{order::v1::Fill, FILE_DESCRIPTOR_SET};
use prost::Message;
use prost_types::{DescriptorProto, FileDescriptorSet};
use std::{collections::BTreeMap, sync::OnceLock};

#[derive(Clone)]
enum Kind {
    Scalar(u8),
    Message(String),
}
type Fields = BTreeMap<u32, Kind>;
type Schema = BTreeMap<String, Fields>;
#[derive(Default, Clone)]
struct Unknowns {
    raw: Vec<(u32, Vec<u8>)>,
    children: BTreeMap<u32, Unknowns>,
}
impl Unknowns {
    fn merge(&mut self, other: Self) {
        self.raw.extend(other.raw);
        for (n, child) in other.children {
            self.children.entry(n).or_default().merge(child);
        }
    }
    fn equivalent(&self, other: &Self) -> bool {
        fn grouped(raw: &[(u32, Vec<u8>)]) -> BTreeMap<u32, Vec<u8>> {
            let mut result: BTreeMap<u32, Vec<u8>> = BTreeMap::new();
            for (n, bytes) in raw {
                result.entry(*n).or_default().extend(bytes);
            }
            result
        }
        if grouped(&self.raw) != grouped(&other.raw) {
            return false;
        }
        for n in self.children.keys().chain(other.children.keys()) {
            let empty = Self::default();
            if !self
                .children
                .get(n)
                .unwrap_or(&empty)
                .equivalent(other.children.get(n).unwrap_or(&empty))
            {
                return false;
            }
        }
        true
    }
}

#[derive(Clone)]
pub struct FillWire {
    value: Fill,
    original: Vec<u8>,
    unknown: Unknowns,
}
impl FillWire {
    pub fn decode(raw: &[u8]) -> Result<Self, Error> {
        if raw.len() > MAX_BYTES {
            return Err(Error::Capacity);
        }
        let (known, unknown) = split(schema()?, "order.v1.Fill", raw, 0)?;
        let value = Fill::decode(known.as_slice()).map_err(|_| Error::InvalidInput)?;
        Ok(Self {
            value,
            original: raw.to_vec(),
            unknown,
        })
    }
    pub(crate) fn previous_fee(&self) -> Result<Self, Error> {
        let previous = self
            .value
            .recovery
            .as_ref()
            .and_then(|r| r.fee_approval.as_ref())
            .and_then(|a| a.previous_fee.as_ref())
            .ok_or(Error::FeeRevision)?;
        let recovery = field_number("order.v1.Fill", "recovery")?;
        let approval = field_number("order.v1.ExecutionRecoveryProvenance", "fee_approval")?;
        let previous_number = field_number("order.v1.ExecutionFeeApproval", "previous_fee")?;
        let fee = field_number("order.v1.Fill", "fee")?;
        let sidecar = self
            .unknown
            .children
            .get(&recovery)
            .and_then(|r| r.children.get(&approval))
            .and_then(|a| a.children.get(&previous_number))
            .cloned()
            .unwrap_or_default();
        let mut result = self.clone();
        result.value.fee = Some(previous.clone());
        result.value.recovery = None;
        result.unknown.children.remove(&recovery);
        result.unknown.children.insert(fee, sidecar);
        result.original = result.journal_bytes()?;
        Ok(result)
    }
    pub(crate) fn without_recovery(&self) -> Result<Self, Error> {
        let mut result = self.clone();
        result.value.recovery = None;
        result
            .unknown
            .children
            .remove(&field_number("order.v1.Fill", "recovery")?);
        result.original = result.journal_bytes()?;
        Ok(result)
    }
    pub fn value(&self) -> &Fill {
        &self.value
    }
    pub fn original(&self) -> &[u8] {
        &self.original
    }
    pub fn equivalent(&self, other: &Self) -> bool {
        self.value == other.value && self.unknown.equivalent(&other.unknown)
    }
    pub fn journal_bytes(&self) -> Result<Vec<u8>, Error> {
        let raw = inject(
            schema()?,
            "order.v1.Fill",
            &self.value.encode_to_vec(),
            &self.unknown,
            0,
        )?;
        if raw.len() > MAX_BYTES {
            return Err(Error::Capacity);
        }
        Ok(raw)
    }
}
fn schema() -> Result<&'static Schema, Error> {
    static SCHEMA: OnceLock<Option<Schema>> = OnceLock::new();
    SCHEMA
        .get_or_init(|| build_schema().ok())
        .as_ref()
        .ok_or(Error::InvalidInput)
}
fn build_schema() -> Result<Schema, Error> {
    let files = FileDescriptorSet::decode(FILE_DESCRIPTOR_SET).map_err(|_| Error::InvalidInput)?;
    schema_from(files)
}
fn schema_from(files: FileDescriptorSet) -> Result<Schema, Error> {
    let mut definitions = BTreeMap::new();
    fn collect(
        prefix: &str,
        messages: &[DescriptorProto],
        out: &mut BTreeMap<String, DescriptorProto>,
    ) {
        for m in messages {
            let name = format!("{}.{}", prefix, m.name());
            out.insert(name.clone(), m.clone());
            collect(&name, &m.nested_type, out);
        }
    }
    for f in files.file {
        collect(f.package(), &f.message_type, &mut definitions);
    }
    let mut result = Schema::new();
    let mut pending = vec![("order.v1.Fill".to_owned(), 0usize)];
    while let Some((name, depth)) = pending.pop() {
        if depth > 16 {
            return Err(Error::InvalidInput);
        }
        if result.contains_key(&name) {
            continue;
        }
        let d = definitions.get(&name).ok_or(Error::InvalidInput)?;
        if !d.oneof_decl.is_empty()
            || !d.extension.is_empty()
            || !d.extension_range.is_empty()
            || d.options.as_ref().is_some_and(|o| o.map_entry())
        {
            return Err(Error::InvalidInput);
        }
        let mut fields = Fields::new();
        for f in &d.field {
            if f.label() != prost_types::field_descriptor_proto::Label::Optional
                || f.oneof_index.is_some()
            {
                return Err(Error::InvalidInput);
            }
            use prost_types::field_descriptor_proto::Type;
            let kind = match f.r#type() {
                Type::Int64 | Type::Int32 | Type::Sint64 | Type::Sint32 | Type::Enum => {
                    Kind::Scalar(0)
                }
                Type::String => Kind::Scalar(2),
                Type::Message => {
                    let child = f.type_name().trim_start_matches('.').to_owned();
                    pending.push((child.clone(), depth + 1));
                    Kind::Message(child)
                }
                _ => return Err(Error::InvalidInput),
            };
            let number = u32::try_from(f.number()).map_err(|_| Error::InvalidInput)?;
            if number == 0 || fields.insert(number, kind).is_some() {
                return Err(Error::InvalidInput);
            }
        }
        result.insert(name, fields);
    }
    Ok(result)
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
fn key(raw: &[u8], pos: &mut usize) -> Result<(u32, u8), Error> {
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
fn payload<'a>(raw: &'a [u8], pos: &mut usize) -> Result<&'a [u8], Error> {
    let n = usize::try_from(varint(raw, pos)?).map_err(|_| Error::InvalidInput)?;
    slice(raw, pos, n)
}
fn skip(raw: &[u8], pos: &mut usize, n: u32, wire: u8) -> Result<(), Error> {
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
fn put_varint(mut v: u64, out: &mut Vec<u8>) {
    while v >= 128 {
        out.push((v as u8 & 127) | 128);
        v >>= 7;
    }
    out.push(v as u8);
}
fn put_message(n: u32, raw: &[u8], out: &mut Vec<u8>) {
    put_varint((u64::from(n) << 3) | 2, out);
    put_varint(raw.len() as u64, out);
    out.extend(raw);
}
fn split(
    schema: &Schema,
    name: &str,
    raw: &[u8],
    depth: usize,
) -> Result<(Vec<u8>, Unknowns), Error> {
    if depth > 16 {
        return Err(Error::InvalidInput);
    }
    let fields = schema.get(name).ok_or(Error::InvalidInput)?;
    let mut known = Vec::new();
    let mut unknown = Unknowns::default();
    let mut pos = 0;
    while pos < raw.len() {
        let start = pos;
        let (n, wire) = key(raw, &mut pos)?;
        let value_start = pos;
        match fields.get(&n) {
            Some(Kind::Message(child)) if wire == 2 => {
                let p = payload(raw, &mut pos)?;
                let (clean, sidecar) = split(schema, child, p, depth + 1)?;
                put_message(n, &clean, &mut known);
                unknown.children.entry(n).or_default().merge(sidecar);
            }
            Some(Kind::Scalar(expected)) if wire == *expected => {
                skip(raw, &mut pos, n, wire)?;
                known.extend(&raw[start..pos]);
            }
            _ => {
                skip(raw, &mut pos, n, wire)?;
                let mut stored = Vec::new();
                put_varint((u64::from(n) << 3) | u64::from(wire), &mut stored);
                stored.extend(&raw[value_start..pos]);
                unknown.raw.push((n, stored));
            }
        }
    }
    Ok((known, unknown))
}
fn inject(
    schema: &Schema,
    name: &str,
    raw: &[u8],
    unknown: &Unknowns,
    depth: usize,
) -> Result<Vec<u8>, Error> {
    if depth > 16 {
        return Err(Error::InvalidInput);
    }
    let fields = schema.get(name).ok_or(Error::InvalidInput)?;
    let mut out = Vec::new();
    let mut pos = 0;
    while pos < raw.len() {
        let start = pos;
        let (n, wire) = key(raw, &mut pos)?;
        if let Some(Kind::Message(child)) = fields.get(&n) {
            if wire != 2 {
                return Err(Error::InvalidInput);
            }
            let p = payload(raw, &mut pos)?;
            let empty = Unknowns::default();
            put_message(
                n,
                &inject(
                    schema,
                    child,
                    p,
                    unknown.children.get(&n).unwrap_or(&empty),
                    depth + 1,
                )?,
                &mut out,
            );
        } else {
            skip(raw, &mut pos, n, wire)?;
            out.extend(&raw[start..pos]);
        }
    }
    for (_, bytes) in &unknown.raw {
        out.extend(bytes);
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn canonical_shape_and_bounds() {
        assert!(schema().is_ok());
        assert!(matches!(
            FillWire::decode(&vec![0; MAX_BYTES + 1]),
            Err(Error::Capacity)
        ));
        for raw in [&[0u8][..], &[0x0c][..], &[0x0b][..], &[0x08, 0x80][..]] {
            assert!(FillWire::decode(raw).is_err());
        }
    }
    #[test]
    fn retained_original_and_go_unknown_tag() {
        let raw = [0xc0, 0xbe, 0x00, 0x81, 0x00];
        let wire = FillWire::decode(&raw).unwrap();
        assert_eq!(wire.original(), raw);
        assert_eq!(wire.journal_bytes().unwrap(), [0xc0, 0x3e, 0x81, 0x00]);
    }
}

#[cfg(test)]
mod shape_tests {
    use super::*;
    #[test]
    fn descriptor_shape_refuses_unreviewed_fields() {
        for change in 0..3 {
            let mut files = FileDescriptorSet::decode(FILE_DESCRIPTOR_SET).unwrap();
            let root = files
                .file
                .iter_mut()
                .filter(|f| f.package() == "order.v1")
                .flat_map(|f| f.message_type.iter_mut())
                .find(|m| m.name() == "Fill")
                .unwrap();
            match change {
                0 => {
                    root.field[0].label =
                        Some(prost_types::field_descriptor_proto::Label::Repeated as i32)
                }
                1 => {
                    root.field[0].r#type =
                        Some(prost_types::field_descriptor_proto::Type::Double as i32)
                }
                _ => root.field[0].oneof_index = Some(0),
            }
            assert!(schema_from(files).is_err());
        }
    }
}

fn field_number(message: &str, field: &str) -> Result<u32, Error> {
    let files = FileDescriptorSet::decode(FILE_DESCRIPTOR_SET).map_err(|_| Error::InvalidInput)?;
    for file in &files.file {
        for m in &file.message_type {
            if format!("{}.{}", file.package(), m.name()) == message {
                for f in &m.field {
                    if f.name() == field {
                        return u32::try_from(f.number()).map_err(|_| Error::InvalidInput);
                    }
                }
            }
        }
    }
    Err(Error::InvalidInput)
}

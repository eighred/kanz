//! Go protobuf normalization: known fields are decoded/merged; unknown value
//! bytes retain their original ordering and representation. No financial model.
use crate::{
    fee_wire::{key, payload, put_message, put_varint, skip},
    Error, MAX_BYTES,
};
use prost::Message;
use prost_reflect::{DynamicMessage, Kind, MessageDescriptor};
use std::collections::BTreeMap;

#[derive(Default)]
struct Unknown {
    raw: Vec<u8>,
    children: BTreeMap<u32, Vec<Unknown>>,
}
impl Unknown {
    fn merge(&mut self, other: Self, desc: &MessageDescriptor) -> Result<(), Error> {
        self.raw.extend(other.raw);
        for (number, mut children) in other.children {
            let field = desc.get_field(number).ok_or(Error::InvalidInput)?;
            let entry = self.children.entry(number).or_default();
            if field.is_list() || entry.is_empty() {
                entry.append(&mut children)
            } else if let Kind::Message(child) = field.kind() {
                entry[0].merge(children.remove(0), &child)?
            } else {
                return Err(Error::InvalidInput);
            }
        }
        Ok(())
    }
}
pub(crate) fn normalize(desc: MessageDescriptor, raw: &[u8]) -> Result<Vec<u8>, Error> {
    if raw.len() > MAX_BYTES {
        return Err(Error::Capacity);
    }
    let (known, unknown) = split(&desc, raw, 0)?;
    let encoded = DynamicMessage::decode(desc.clone(), known.as_slice())
        .map_err(|_| Error::InvalidInput)?
        .encode_to_vec();
    let out = inject(&desc, &encoded, &unknown, 0)?;
    if out.len() > MAX_BYTES {
        return Err(Error::Capacity);
    }
    Ok(out)
}
fn wire(kind: &Kind) -> u8 {
    match kind {
        Kind::Double | Kind::Fixed64 | Kind::Sfixed64 => 1,
        Kind::Float | Kind::Fixed32 | Kind::Sfixed32 => 5,
        Kind::String | Kind::Bytes | Kind::Message(_) => 2,
        _ => 0,
    }
}
fn split(desc: &MessageDescriptor, raw: &[u8], depth: usize) -> Result<(Vec<u8>, Unknown), Error> {
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
        let supported = field.as_ref().is_some_and(|f| {
            w == wire(&f.kind()) || (f.is_list() && w == 2 && wire(&f.kind()) != 2)
        });
        if supported {
            let field = field.ok_or(Error::InvalidInput)?;
            // Current OMS FACT graphs have neither maps nor oneofs. Refuse a
            // future extension until its overwrite/unknown semantics is qualified.
            if field.is_map() || field.containing_oneof().is_some() {
                return Err(Error::InvalidInput);
            }
            if let Kind::Message(child) = field.kind() {
                let bytes = payload(raw, &mut pos)?;
                let (clean, sidecar) = split(&child, bytes, depth + 1)?;
                put_message(number, &clean, &mut known);
                let entries = unknown.children.entry(number).or_default();
                if field.is_list() || entries.is_empty() {
                    entries.push(sidecar)
                } else {
                    entries[0].merge(sidecar, &child)?
                }
            } else {
                skip(raw, &mut pos, number, w)?;
                known.extend_from_slice(&raw[start..pos]);
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
                put_message(
                    number,
                    &inject(&child, bytes, sidecar, depth + 1)?,
                    &mut result,
                );
                *i += 1;
                continue;
            }
        }
        skip(raw, &mut pos, number, w)?;
        result.extend_from_slice(&raw[start..pos]);
    }
    result.extend_from_slice(&unknown.raw);
    Ok(result)
}

#[cfg(test)]
mod tests {
    use super::*;
    use prost_reflect::DescriptorPool;
    fn decode(name: &str, raw: &[u8]) -> Vec<u8> {
        let pool =
            DescriptorPool::decode(kanz_oms_fold_shadow::proto::FILE_DESCRIPTOR_SET).unwrap();
        normalize(pool.get_message_by_name(name).unwrap(), raw).unwrap()
    }
    #[test]
    fn known_noncanonical_values_normalize_unknown_values_do_not() {
        assert_eq!(
            decode(
                "common.v1.Decimal",
                &[0xa0, 0x86, 0x00, 0x81, 0, 8, 0x82, 0]
            ),
            vec![8, 2, 0xa0, 6, 0x81, 0]
        );
    }
    #[test]
    fn duplicate_embedded_messages_merge_unknowns_in_encounter_order() {
        assert_eq!(
            decode(
                "common.v1.Money",
                &[10, 5, 8, 2, 0xa0, 6, 1, 10, 5, 16, 4, 0x98, 6, 2]
            ),
            vec![10, 10, 8, 2, 16, 4, 0xa0, 6, 1, 0x98, 6, 2]
        );
    }
    #[test]
    fn repeated_messages_keep_separate_unknowns() {
        let raw = [
            26, 6, 10, 1, b'a', 0xa0, 6, 1, 26, 6, 10, 1, b'b', 0xa0, 6, 2,
        ];
        assert_eq!(decode("order.v1.ExecutionRecoveryHistory", &raw), raw);
    }
    #[test]
    fn malformed_payload_refuses() {
        let pool =
            DescriptorPool::decode(kanz_oms_fold_shadow::proto::FILE_DESCRIPTOR_SET).unwrap();
        assert!(normalize(
            pool.get_message_by_name("common.v1.Money").unwrap(),
            &[10, 4, 8]
        )
        .is_err());
    }
}

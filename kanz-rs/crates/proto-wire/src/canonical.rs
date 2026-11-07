//! Immutable generated value and its Go-compatible canonical wire.
use crate::{normalize, Error, MAX_BYTES};
use prost::Message;
use prost_reflect::MessageDescriptor;

pub trait Schema: Message + Default + Send + 'static {
    const NAME: &'static str;
    fn descriptor() -> Result<MessageDescriptor, Error>;
}

#[derive(Debug, Clone)]
pub struct Canonical<T> {
    value: T,
    wire: Vec<u8>,
}
impl<T: Schema> Canonical<T> {
    pub fn decode(raw: &[u8]) -> Result<Self, Error> {
        let wire = normalize(T::descriptor()?, raw)?;
        // Prost rejects some Go-valid unknown encodings sharing a known number.
        // Decode only descriptor-known bytes; the complete wire remains immutable.
        let (known, _) = crate::split(&T::descriptor()?, &wire, 0)?;
        let value = T::decode(known.as_slice()).map_err(|_| Error::InvalidInput)?;
        Ok(Self { value, wire })
    }
    pub fn from_value(value: T) -> Result<Self, Error> {
        if value.encoded_len() > MAX_BYTES {
            return Err(Error::Capacity);
        }
        Self::decode(&value.encode_to_vec())
    }
    pub fn value(&self) -> &T {
        &self.value
    }
    pub fn wire(&self) -> &[u8] {
        &self.wire
    }
    /// Raw singular embedded message; preserves absence versus an empty message.
    pub fn message_wire(&self, name: &str) -> Result<Option<Vec<u8>>, Error> {
        let field = T::descriptor()?
            .get_field_by_name(name)
            .ok_or(Error::InvalidInput)?;
        if field.is_list() || !matches!(field.kind(), prost_reflect::Kind::Message(_)) {
            return Err(Error::InvalidInput);
        }
        let mut pos = 0;
        while pos < self.wire.len() {
            let (n, w) = crate::key(&self.wire, &mut pos)?;
            if n == field.number() && w == 2 {
                return Ok(Some(crate::payload(&self.wire, &mut pos)?.to_vec()));
            }
            crate::skip(&self.wire, &mut pos, n, w)?;
        }
        Ok(None)
    }
}

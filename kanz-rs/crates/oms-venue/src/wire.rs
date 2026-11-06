//! Generated values plus their canonical Go-compatible wire, including unknowns.
use bytes::{Buf, BufMut};
#[cfg(test)]
use prost::Message;
use prost_reflect::{DescriptorPool, MessageDescriptor};
use std::{marker::PhantomData, sync::OnceLock};
use tonic::{
    codec::{Codec, DecodeBuf, Decoder, EncodeBuf, Encoder},
    Status,
};

pub use kanz_proto_wire::Schema;
pub(crate) fn descriptor<T: Schema>() -> Result<MessageDescriptor, Status> {
    T::descriptor().map_err(status)
}
fn status(error: kanz_proto_wire::Error) -> Status {
    match error {
        kanz_proto_wire::Error::Capacity => Status::resource_exhausted("canonical message bound"),
        kanz_proto_wire::Error::InvalidInput => {
            Status::invalid_argument("invalid canonical message")
        }
    }
}
/// Transport facade delegates immutable storage/normalization to the pure owner.
#[derive(Debug, Clone)]
pub struct Canonical<T>(kanz_proto_wire::Canonical<T>);
impl<T: Schema> Canonical<T> {
    pub fn decode(raw: &[u8]) -> Result<Self, Status> {
        kanz_proto_wire::Canonical::decode(raw)
            .map(Self)
            .map_err(status)
    }
    fn decode_with_budget(raw: &[u8], budget: kanz_proto_wire::Budget) -> Result<Self, Status> {
        kanz_proto_wire::Canonical::decode_with_budget(raw, budget)
            .map(Self)
            .map_err(status)
    }
    pub fn from_value(value: T) -> Result<Self, Status> {
        kanz_proto_wire::Canonical::from_value(value)
            .map(Self)
            .map_err(status)
    }
    pub fn value(&self) -> &T {
        self.0.value()
    }
    pub fn wire(&self) -> &[u8] {
        self.0.wire()
    }
}
impl<T> From<kanz_proto_wire::Canonical<T>> for Canonical<T> {
    fn from(value: kanz_proto_wire::Canonical<T>) -> Self {
        Self(value)
    }
}
fn venue_descriptor(name: &str) -> Result<MessageDescriptor, kanz_proto_wire::Error> {
    static POOL: OnceLock<Result<DescriptorPool, prost_reflect::DescriptorError>> = OnceLock::new();
    POOL.get_or_init(|| DescriptorPool::decode(crate::proto::FILE_DESCRIPTOR_SET))
        .as_ref()
        .map_err(|_| kanz_proto_wire::Error::InvalidInput)?
        .get_message_by_name(name)
        .ok_or(kanz_proto_wire::Error::InvalidInput)
}
macro_rules! schema {
    ($ty:path,$name:literal) => {
        impl Schema for $ty {
            const NAME: &'static str = $name;
            fn descriptor() -> Result<MessageDescriptor, kanz_proto_wire::Error> {
                venue_descriptor(Self::NAME)
            }
        }
    };
}
schema!(
    crate::proto::venue::v1::ExecuteRequest,
    "venue.v1.ExecuteRequest"
);
schema!(
    crate::proto::venue::v1::ExecuteResponse,
    "venue.v1.ExecuteResponse"
);
schema!(
    crate::proto::venue::v1::CancelOrderRequest,
    "venue.v1.CancelOrderRequest"
);
schema!(
    crate::proto::venue::v1::CancelOrderResponse,
    "venue.v1.CancelOrderResponse"
);
schema!(
    crate::proto::venue::v1::DescribeRequest,
    "venue.v1.DescribeRequest"
);
schema!(
    crate::proto::venue::v1::DescribeResponse,
    "venue.v1.DescribeResponse"
);
schema!(
    crate::proto::venue::v1::ListInstrumentsRequest,
    "venue.v1.ListInstrumentsRequest"
);
schema!(
    crate::proto::venue::v1::ListInstrumentsResponse,
    "venue.v1.ListInstrumentsResponse"
);
schema!(
    crate::proto::venue::v1::QueryOrderRequest,
    "venue.v1.QueryOrderRequest"
);
schema!(
    crate::proto::venue::v1::QueryOrderResponse,
    "venue.v1.QueryOrderResponse"
);
pub(crate) struct WireCodec<A, B>(PhantomData<(A, B)>, kanz_proto_wire::Budget);
impl<A, B> Default for WireCodec<A, B> {
    fn default() -> Self {
        Self(PhantomData, kanz_proto_wire::Budget::Standard)
    }
}
impl<A, B> WireCodec<A, B> {
    pub(crate) fn recovery_query() -> Self {
        Self(PhantomData, kanz_proto_wire::Budget::RecoveryHistory)
    }
}
pub(crate) struct WireEncoder<A>(PhantomData<A>);
pub(crate) struct WireDecoder<B>(PhantomData<B>, kanz_proto_wire::Budget);
impl<A: Schema, B: Schema> Codec for WireCodec<A, B> {
    type Encode = Canonical<A>;
    type Decode = Canonical<B>;
    type Encoder = WireEncoder<A>;
    type Decoder = WireDecoder<B>;
    fn encoder(&mut self) -> Self::Encoder {
        WireEncoder(PhantomData)
    }
    fn decoder(&mut self) -> Self::Decoder {
        WireDecoder(PhantomData, self.1)
    }
}
impl<A: Schema> Encoder for WireEncoder<A> {
    type Item = Canonical<A>;
    type Error = Status;
    fn encode(&mut self, item: Self::Item, dst: &mut EncodeBuf<'_>) -> Result<(), Status> {
        dst.put_slice(item.wire());
        Ok(())
    }
}
impl<B: Schema> Decoder for WireDecoder<B> {
    type Item = Canonical<B>;
    type Error = Status;
    fn decode(&mut self, src: &mut DecodeBuf<'_>) -> Result<Option<Self::Item>, Status> {
        if src.remaining() > self.1.bytes() {
            return Err(Status::resource_exhausted("canonical message bound"));
        }
        let raw = src.copy_to_bytes(src.remaining());
        Canonical::decode_with_budget(&raw, self.1).map(Some)
    }
}

/// Replace one generated embedded-message field with its preserved canonical wire.
/// The descriptor binds the field name/type; no parallel financial wire model.
pub(crate) fn with_state<T: Schema>(
    value: T,
    state: &Canonical<kanz_order_core::proto::order::v1::OrderState>,
) -> Result<Canonical<T>, Status> {
    let desc = descriptor::<T>()?;
    let field = desc
        .get_field_by_name("state")
        .ok_or_else(|| Status::internal("state descriptor unavailable"))?;
    if !matches!(field.kind(),prost_reflect::Kind::Message(ref d) if d.full_name()=="order.v1.OrderState")
    {
        return Err(Status::internal("state descriptor mismatch"));
    }
    let raw = value.encode_to_vec();
    let mut out = Vec::new();
    let mut pos = 0;
    while pos < raw.len() {
        let start = pos;
        let (n, w) = kanz_proto_wire::key(&raw, &mut pos)
            .map_err(|_| Status::internal("generated wire invalid"))?;
        kanz_proto_wire::skip(&raw, &mut pos, n, w)
            .map_err(|_| Status::internal("generated wire invalid"))?;
        if n != field.number() {
            out.extend_from_slice(&raw[start..pos]);
        }
    }
    kanz_proto_wire::put_message(field.number(), state.wire(), &mut out);
    Canonical::decode(&out)
}
/// Extract canonical nested messages after duplicate singular merge normalization.
pub(crate) fn messages<A: Schema, B: Schema>(
    value: &Canonical<A>,
    name: &str,
) -> Result<Vec<Canonical<B>>, Status> {
    messages_budget(value, name, kanz_proto_wire::Budget::Standard)
}
pub(crate) fn messages_budget<A: Schema, B: Schema>(
    value: &Canonical<A>,
    name: &str,
    budget: kanz_proto_wire::Budget,
) -> Result<Vec<Canonical<B>>, Status> {
    if budget == kanz_proto_wire::Budget::RecoveryHistory
        && (A::NAME != "venue.v1.QueryOrderResponse"
            || B::NAME != "order.v1.Fill"
            || name != "fills")
    {
        return Err(Status::invalid_argument("recovery nested response scope"));
    }
    let field = descriptor::<A>()?
        .get_field_by_name(name)
        .ok_or_else(|| Status::internal("field descriptor unavailable"))?;
    if !matches!(field.kind(),prost_reflect::Kind::Message(ref d) if d.full_name()==B::NAME) {
        return Err(Status::internal("field descriptor mismatch"));
    }
    let raw = value.wire();
    let mut pos = 0;
    let mut out = Vec::new();
    while pos < raw.len() {
        let (n, w) = kanz_proto_wire::key(raw, &mut pos)
            .map_err(|_| Status::internal("normalized wire invalid"))?;
        if n == field.number() && w == 2 {
            let nested = kanz_proto_wire::payload(raw, &mut pos)
                .map_err(|_| Status::internal("normalized wire invalid"))?;
            out.push(Canonical::decode_with_budget(nested, budget)?);
        } else {
            kanz_proto_wire::skip(raw, &mut pos, n, w)
                .map_err(|_| Status::internal("normalized wire invalid"))?;
        }
    }
    Ok(out)
}

impl Canonical<crate::proto::venue::v1::ExecuteResponse> {
    pub fn fills(&self) -> Result<Vec<Canonical<kanz_order_core::proto::order::v1::Fill>>, Status> {
        messages(self, "fills")
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn recovery_query_budget_reaches_nested_fill_only() {
        use crate::proto::venue::v1::QueryOrderResponse;
        use kanz_order_core::proto::order::v1::Fill;
        let mut fill = Fill {
            fill_id: "known".into(),
            ..Default::default()
        }
        .encode_to_vec();
        kanz_proto_wire::put_message(199, &vec![b'x'; kanz_proto_wire::MAX_BYTES + 1], &mut fill);
        let mut raw = Vec::new();
        kanz_proto_wire::put_message(2, &fill, &mut raw);
        assert_eq!(
            Canonical::<QueryOrderResponse>::decode(&raw)
                .unwrap_err()
                .code(),
            tonic::Code::ResourceExhausted
        );
        let response = Canonical::<QueryOrderResponse>::decode_with_budget(
            &raw,
            kanz_proto_wire::Budget::RecoveryHistory,
        )
        .unwrap();
        let nested: Vec<Canonical<Fill>> =
            messages_budget(&response, "fills", kanz_proto_wire::Budget::RecoveryHistory).unwrap();
        assert_eq!(nested.len(), 1);
        assert_eq!(nested[0].wire(), fill);
        assert!(messages::<_, Fill>(&response, "fills").is_err());
        assert!(Canonical::<QueryOrderResponse>::decode_with_budget(
            &vec![0; 4 * kanz_proto_wire::MAX_BYTES + 1],
            kanz_proto_wire::Budget::RecoveryHistory
        )
        .is_err());
    }
    #[test]
    fn recovery_count_bound_is_complete_or_refused() {
        use crate::proto::venue::v1::QueryOrderResponse;
        let mut raw = Vec::new();
        for _ in 0..8000 {
            kanz_proto_wire::put_message(2, &[], &mut raw);
        }
        let response = Canonical::<QueryOrderResponse>::decode_with_budget(
            &raw,
            kanz_proto_wire::Budget::RecoveryHistory,
        )
        .unwrap();
        assert_eq!(response.value().fills.len(), 8000);
        kanz_proto_wire::put_message(2, &[], &mut raw);
        // One sentinel above the accepted history limit reaches CompleteHistory
        // and its durable blocked outcome; never return an accepted prefix.
        assert_eq!(
            Canonical::<QueryOrderResponse>::decode_with_budget(
                &raw,
                kanz_proto_wire::Budget::RecoveryHistory
            )
            .unwrap()
            .value()
            .fills
            .len(),
            8001
        );
        kanz_proto_wire::put_message(2, &[], &mut raw);
        assert_eq!(
            Canonical::<QueryOrderResponse>::decode_with_budget(
                &raw,
                kanz_proto_wire::Budget::RecoveryHistory
            )
            .unwrap_err()
            .code(),
            tonic::Code::ResourceExhausted
        );
    }
    use crate::proto::venue::v1::{ExecuteRequest, ExecuteResponse};
    use kanz_order_core::proto::order::v1::OrderState;
    #[test]
    fn nested_request_and_fill_keep_unknown_value_bytes() {
        let mut raw = OrderState {
            order_id: "order".into(),
            ..Default::default()
        }
        .encode_to_vec();
        raw.extend([0xa0, 6, 0x81, 0]);
        let state = Canonical::<OrderState>::decode(&raw).unwrap();
        let request = with_state(
            ExecuteRequest {
                tenant_id: "tenant-a".into(),
                state: None,
            },
            &state,
        )
        .unwrap();
        let states: Vec<Canonical<OrderState>> = messages(&request, "state").unwrap();
        assert_eq!(states[0].wire(), raw);
        let mut response = Vec::new();
        kanz_proto_wire::put_message(1, &[0xa0, 6, 0x81, 0], &mut response);
        assert_eq!(
            Canonical::<ExecuteResponse>::decode(&response)
                .unwrap()
                .fills()
                .unwrap()[0]
                .wire(),
            [0xa0, 6, 0x81, 0]
        );
    }
    #[test]
    fn canonical_capacity_precedes_allocation_and_malformed_refuses() {
        let state = OrderState {
            order_id: "x".repeat(kanz_proto_wire::MAX_BYTES),
            ..Default::default()
        };
        assert_eq!(
            Canonical::from_value(state).unwrap_err().code(),
            tonic::Code::ResourceExhausted
        );
        assert!(Canonical::<OrderState>::decode(&[0]).is_err());
    }
    #[test]
    fn generated_rpc_contract_is_exact() {
        let pool = DescriptorPool::decode(crate::proto::FILE_DESCRIPTOR_SET).unwrap();
        let service = pool
            .services()
            .find(|s| s.full_name() == "venue.v1.VenueAdapterService")
            .unwrap();
        let names = service
            .methods()
            .map(|m| m.name().to_owned())
            .collect::<Vec<_>>();
        assert_eq!(
            names,
            [
                "Execute",
                "CancelOrder",
                "Describe",
                "ListInstruments",
                "QueryOrder"
            ]
        );
    }
}

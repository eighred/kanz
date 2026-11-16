#![forbid(unsafe_code)]
//! Pure canonical OMS aggregate; no transport, persistence or admission authority.
pub mod proto {
    pub mod domain {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/domain.v1.rs"));
        }
    }
    pub mod envelope {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/envelope.v1.rs"));
        }
    }
    pub const FILE_DESCRIPTOR_SET: &[u8] =
        include_bytes!(concat!(env!("OUT_DIR"), "/order-descriptors.bin"));
    pub use kanz_decimal_compat::common;
    pub mod command {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/command.v1.rs"));
        }
    }
    pub mod market {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/market.v1.rs"));
        }
    }
    pub mod order {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/order.v1.rs"));
        }
    }
}
pub mod aggregate;
pub mod arithmetic;
pub mod attribution;
pub mod attribution_projection;
pub mod schedule_identity;
pub use arithmetic::{decimal, exact_decimal, has_terminating_decimal, rational};

/// Whole-message canonical Decimal domain, shared by projector and recovery
/// preparation. Unknown fields are retained, never interpreted as money.
pub fn decimal_domain(
    message: &prost_reflect::DynamicMessage,
    depth: usize,
) -> Result<(), kanz_proto_wire::Error> {
    use prost_reflect::{ReflectMessage, Value};
    if depth > 32 {
        return Err(kanz_proto_wire::Error::Capacity);
    }
    if message.descriptor().full_name() == "common.v1.Decimal"
        && matches!(message.get_field_by_name("exponent").as_deref(),Some(Value::I32(n)) if !(-64..=64).contains(n))
    {
        return Err(kanz_proto_wire::Error::InvalidInput);
    }
    for (_, value) in message.fields() {
        match value {
            Value::Message(m) => decimal_domain(m, depth + 1)?,
            Value::List(values) => {
                for value in values {
                    if let Value::Message(m) = value {
                        decimal_domain(m, depth + 1)?;
                    }
                }
            }
            _ => {}
        }
    }
    Ok(())
}

pub use kanz_proto_wire::Canonical;
use prost_reflect::{DescriptorPool, MessageDescriptor};
use std::sync::OnceLock;
fn descriptor(name: &str) -> Result<MessageDescriptor, kanz_proto_wire::Error> {
    static POOL: OnceLock<Result<DescriptorPool, prost_reflect::DescriptorError>> = OnceLock::new();
    POOL.get_or_init(|| DescriptorPool::decode(proto::FILE_DESCRIPTOR_SET))
        .as_ref()
        .map_err(|_| kanz_proto_wire::Error::InvalidInput)?
        .get_message_by_name(name)
        .ok_or(kanz_proto_wire::Error::InvalidInput)
}
macro_rules! schema {
    ($ty:path,$name:literal) => {
        impl kanz_proto_wire::Schema for $ty {
            const NAME: &'static str = $name;
            fn descriptor() -> Result<MessageDescriptor, kanz_proto_wire::Error> {
                descriptor(Self::NAME)
            }
        }
    };
}
schema!(proto::order::v1::OrderState, "order.v1.OrderState");
schema!(proto::order::v1::Fill, "order.v1.Fill");
schema!(proto::order::v1::SubmitOrder, "order.v1.SubmitOrder");
schema!(proto::order::v1::AmendOrder, "order.v1.AmendOrder");
schema!(
    proto::order::v1::ExecutionAttributionRecorded,
    "order.v1.ExecutionAttributionRecorded"
);

schema!(proto::order::v1::StateHealed, "order.v1.StateHealed");
schema!(
    proto::order::v1::ExecutionRecoveryHistory,
    "order.v1.ExecutionRecoveryHistory"
);
schema!(
    proto::order::v1::ExecutionRecoveryRecorded,
    "order.v1.ExecutionRecoveryRecorded"
);
schema!(
    proto::order::v1::ExecutionRecovered,
    "order.v1.ExecutionRecovered"
);
schema!(
    proto::order::v1::ExecutionRecoveryApplied,
    "order.v1.ExecutionRecoveryApplied"
);
schema!(
    proto::order::v1::ExecutionFeeCorrectionProposed,
    "order.v1.ExecutionFeeCorrectionProposed"
);
schema!(
    proto::order::v1::ExecutionFeeCorrectionApproved,
    "order.v1.ExecutionFeeCorrectionApproved"
);
schema!(
    proto::order::v1::ProposeExecutionFeeCorrection,
    "order.v1.ProposeExecutionFeeCorrection"
);
schema!(
    proto::order::v1::ApproveExecutionFeeCorrection,
    "order.v1.ApproveExecutionFeeCorrection"
);
schema!(proto::envelope::v1::Envelope, "envelope.v1.Envelope");
schema!(
    proto::order::v1::ExecutionFeeChange,
    "order.v1.ExecutionFeeChange"
);
schema!(
    proto::order::v1::ExecutionFeeApproval,
    "order.v1.ExecutionFeeApproval"
);
schema!(
    proto::order::v1::ExecutionRecoveryProvenance,
    "order.v1.ExecutionRecoveryProvenance"
);

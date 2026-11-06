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
pub use arithmetic::{decimal, rational};

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

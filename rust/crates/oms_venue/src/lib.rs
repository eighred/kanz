#![forbid(unsafe_code)]
//! Typed venue adapter client qualification. No admission or exchange credentials.
mod client;
mod transport;
mod wire;
pub use client::{Client, OrderView, Registration, Requirements, ViewState};
pub use transport::Config;
pub use wire::{Canonical, Schema};
pub mod proto {
    pub mod venue {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/venue.v1.rs"));
        }
    }
    pub const FILE_DESCRIPTOR_SET: &[u8] =
        include_bytes!(concat!(env!("OUT_DIR"), "/venue-descriptors.bin"));
}

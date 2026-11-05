#![forbid(unsafe_code)]
//! Public market-data ingestion only. No venue credentials or order authority.
pub mod proto {
    pub use kanz_decimal_compat::common;
    pub mod market {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/market.v1.rs"));
        }
    }
    pub mod envelope {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/envelope.v1.rs"));
        }
    }
}
pub mod book;
pub mod decimal;
pub mod publisher;
pub mod source;

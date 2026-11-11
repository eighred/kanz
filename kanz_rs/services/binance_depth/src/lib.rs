#![forbid(unsafe_code)]
//! Binance transport over the shared canonical market-feed runtime.
pub use kanz_market_feed::{book, decimal, proto, publisher};
pub mod source;

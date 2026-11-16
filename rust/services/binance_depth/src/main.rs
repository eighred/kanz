#![forbid(unsafe_code)]
use kanz_binance_depth::source;
use kanz_market_feed::{
    publisher::Venue,
    runtime::{self, Config, Result},
};
use std::env;

async fn run() -> Result<()> {
    let config = Config::from_env(
        "BINANCE_DEPTH",
        Venue::Binance,
        option_env!("KANZ_BUILD_VERSION"),
    )?;
    let source_config = source::Config {
        instrument_id: config.instrument_id.clone(),
        symbol: config.symbol.clone(),
        mic: "BINANCE".into(),
        depth_limit: 1000,
        allow_local_test: config.development,
        ws_base: env::var("BINANCE_DEPTH_WS_BASE")
            .unwrap_or_else(|_| "wss://stream.binance.com:9443".into()),
        rest_base: env::var("BINANCE_DEPTH_REST_BASE")
            .unwrap_or_else(|_| "https://api.binance.com".into()),
    };
    source_config.validate()?;
    runtime::run(config, move |sender, cancel, validity| {
        source::run(source_config, sender, cancel, validity)
    })
    .await
}
#[tokio::main]
async fn main() {
    runtime::install_crypto_provider();
    if let Err(error) = run().await {
        eprintln!("binance-depth stopped: {error}");
        std::process::exit(1);
    }
}

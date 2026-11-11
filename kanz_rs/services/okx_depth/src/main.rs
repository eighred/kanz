#![forbid(unsafe_code)]
use kanz_market_feed::{
    publisher::Venue,
    runtime::{self, Config, Result},
};
use kanz_okx_depth::source;

async fn run() -> Result<()> {
    let config = Config::from_env("OKX_DEPTH", Venue::Okx, option_env!("KANZ_BUILD_VERSION"))?;
    let source_config = source::Config {
        instrument_id: config.instrument_id.clone(),
        symbol: config.symbol.clone(),
        allow_local_test: config.development,
        ws_url: std::env::var("OKX_DEPTH_WS_URL")
            .unwrap_or_else(|_| "wss://ws.okx.com:8443/ws/v5/public".into()),
    };
    source_config.validate()?;
    runtime::run(config, move |sender, cancel, validity| {
        source::run(source_config, sender, cancel, validity)
    })
    .await
}
#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("okx-depth stopped: {error}");
        std::process::exit(1)
    }
}

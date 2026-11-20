package binance

const fixtureAccount = "fixture-account"

// Isolated conversion tests supply the already verified account. The startup
// and signed-stream tests exercise the exchange proof on a loopback server.
func provenBinanceFixture(cfg BinanceConfig) *BinanceVenue {
	cfg.Account = fixtureAccount
	v := NewBinanceVenue(cfg)
	v.accountVerified.Store(true)
	return v
}

func fixtureUserDataIngester(cfg UserDataConfig) *UserDataIngester {
	cfg.provenAccount = fixtureAccount
	return newUserDataIngester(cfg)
}

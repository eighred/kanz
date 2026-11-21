package binance

import (
	"context"
	"errors"
	"strings"

	"github.com/eighred/kanz/internal/execution"
)

// DemoSpotFunds reads only the explicitly reported free balances at Spot Testnet.
func (v *BinanceVenue) DemoSpotFunds(ctx context.Context, in execution.InstrumentSymbol, uid string) (string, string, error) {
	if strings.TrimRight(v.rest.baseURL, "/") != "https://testnet.binance.vision" || uid == "" ||
		in.VenueSymbol != in.Pair.Base+in.Pair.Quote {
		return "", "", errors.New("unavailable Binance Spot Testnet pair")
	}
	info, err := v.rest.account(ctx)
	if err != nil {
		return "", "", err
	}
	if info.AccountType != "SPOT" || !info.CanTrade || string(info.UID) != uid {
		return "", "", errors.New("spot account identity or trading status unavailable")
	}
	funds := make(map[string]string, 2)
	for _, row := range info.Balances {
		if row.Asset == in.Pair.Base || row.Asset == in.Pair.Quote {
			if _, found := funds[row.Asset]; found {
				return "", "", errors.New("ambiguous spot account balance")
			}
			funds[row.Asset] = row.Free
		}
	}
	return funds[in.Pair.Base], funds[in.Pair.Quote], nil
}

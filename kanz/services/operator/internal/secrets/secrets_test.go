package secrets

import "testing"

func TestValidateVenueKeys(t *testing.T) {
	cases := []struct {
		name    string
		venue   string
		keys    VenueKeys
		wantErr bool
	}{
		{"okx ok", "okx", VenueKeys{"k", "s", "p"}, false},
		{"okx no passphrase", "okx", VenueKeys{"k", "s", ""}, true},
		{"binance ok", "binance", VenueKeys{"k", "s", ""}, false},
		{"binance with passphrase", "binance", VenueKeys{"k", "s", "p"}, true},
		{"missing secret", "okx", VenueKeys{"k", "", "p"}, true},
		{"unknown venue", "kraken", VenueKeys{"k", "s", ""}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateVenueKeys(c.venue, c.keys)
			if (err != nil) != c.wantErr {
				t.Errorf("ValidateVenueKeys(%q) err=%v, wantErr=%v", c.venue, err, c.wantErr)
			}
		})
	}
}

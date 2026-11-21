package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/marketdata/terms"
	"github.com/eighred/kanz/pkg/secret"
	"github.com/eighred/kanz/services/market_data/internal/config"
	"google.golang.org/protobuf/encoding/protojson"
)

// readOptionTerms is an offline operator read over the configured market-data
// store. It does not start the service, subscribe to a feed or fetch a contract.
func readOptionTerms(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("market-data option-terms", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Read one local market-data ContractTerms revision as JSON using MARKET_DATA_DATABASE_URL_FILE or MARKET_DATA_DATABASE_URL.")
		fmt.Fprintln(flags.Output(), "The ingestion cutoff uses the stored transaction timestamp. Supply --sha256 to require the same bytes on replay.")
		flags.PrintDefaults()
	}
	instrument := flags.String("instrument", "", "canonical option instrument ID")
	effective := flags.String("effective-at", "", "inclusive effective cutoff, RFC3339")
	ingested := flags.String("ingested-by", "", "inclusive database ingestion cutoff, RFC3339")
	expected := flags.String("sha256", "", "require this stored ContractTerms SHA-256")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *instrument == "" || strings.TrimSpace(*instrument) != *instrument {
		return errors.New("option-terms: a canonical --instrument and no positional arguments are required")
	}
	asOf, err := parseTermsCutoff(*effective, "--effective-at")
	if err != nil {
		return err
	}
	knownBy, err := parseTermsCutoff(*ingested, "--ingested-by")
	if err != nil {
		return err
	}
	if *expected != "" {
		b, err := hex.DecodeString(*expected)
		if err != nil || len(b) != 32 {
			return errors.New("option-terms: --sha256 requires a 64-digit hex digest")
		}
	}
	dsn, err := secret.Read("MARKET_DATA_DATABASE_URL")
	if err != nil || dsn == "" {
		return errors.New("option-terms: configure the market-data database connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := openMarketPool(ctx, config.Config{DatabaseURL: dsn})
	if err != nil {
		return errors.New("option-terms: cannot open configured market-data database")
	}
	defer pool.Close()

	version, err := terms.NewPostgres(pool).OptionAt(ctx, *instrument, asOf, knownBy)
	if errors.Is(err, terms.ErrNoTerms) || errors.Is(err, terms.ErrNotOption) {
		return err
	}
	if err != nil {
		return errors.New("option-terms: cannot read a valid stored contract revision")
	}
	digest := hex.EncodeToString(version.SHA256[:])
	if *expected != "" && !strings.EqualFold(*expected, digest) {
		return errors.New("option-terms: selected revision does not match --sha256")
	}
	contract, err := protojson.Marshal(version.Terms)
	if err != nil {
		return fmt.Errorf("option-terms: encode contract: %w", err)
	}
	return json.NewEncoder(out).Encode(struct {
		EffectiveAt time.Time       `json:"effective_at"`
		IngestedBy  time.Time       `json:"ingested_by"`
		IngestedAt  time.Time       `json:"ingested_at"`
		SHA256      string          `json:"sha256"`
		Contract    json.RawMessage `json:"contract"`
	}{asOf.UTC(), knownBy.UTC(), version.IngestedAt.UTC(), digest, contract})
}

func parseTermsCutoff(raw, flagName string) (time.Time, error) {
	// time.Parse silently truncates fractional digits beyond nanoseconds.
	// Count the supplied digits before the database can lose that distinction.
	if fraction := strings.IndexAny(raw, ".,"); fraction >= 0 {
		digits := raw[fraction+1:]
		if zone := strings.IndexAny(digits, "Z+-"); zone >= 0 {
			digits = digits[:zone]
		}
		if len(digits) > 6 {
			return time.Time{}, fmt.Errorf("option-terms: %s exceeds microsecond precision", flagName)
		}
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || at.IsZero() {
		return time.Time{}, fmt.Errorf("option-terms: %s requires an RFC3339 timestamp with at most microsecond precision", flagName)
	}
	return at, nil
}

// Command testnet_paper evaluates one archived BTCUSDT testnet day offline.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

func main() {
	input := flag.String("input", "", "Archived Binance Spot Testnet BTCUSDT 5m kline JSON file")
	day := flag.String("day", "", "Completed evaluation day in UTC (YYYY-MM-DD)")
	flag.Parse()
	if err := run(*input, *day); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(path, label string) error {
	day, err := time.Parse(time.DateOnly, label)
	if err != nil || !day.Add(24*time.Hour).Before(time.Now()) {
		return fmt.Errorf("-day must be a completed UTC day")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	const maxBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxBytes {
		return fmt.Errorf("input exceeds 1 MiB")
	}
	bars, err := readBars(data, day)
	if err != nil {
		return err
	}
	r, err := simulate(context.Background(), bars)
	if err != nil {
		return err
	}
	r.Hash = fmt.Sprintf("%x", sha256.Sum256(data))
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Command option_gpu_comparison is the CPU baseline for the eight-contract
// diagnostic workload. Its only I/O is one JSON request/response per line.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/eighred/kanz/internal/risk/pricing"
)

func run() error {
	in := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var rows [][]float64
		if err := json.Unmarshal(in.Bytes(), &rows); err != nil {
			return err
		}
		if len(rows) != 8 {
			return fmt.Errorf("expected eight reference contracts")
		}
		result := make([]float64, len(rows))
		var group sync.WaitGroup
		for index, row := range rows {
			if len(row) != 7 || (row[0] != 1 && row[0] != -1) {
				return fmt.Errorf("invalid reference contract")
			}
			right := pricing.Call
			if row[0] == -1 {
				right = pricing.Put
			}
			group.Go(func() {
				result[index] = pricing.Price(right, pricing.American, row[1], row[2], row[3], row[4], row[5], row[6])
			})
		}
		group.Wait()
		if err := out.Encode(result); err != nil {
			return err
		}
	}
	return in.Err()
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

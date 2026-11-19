// Run from the Kanz Go module with the generated schema SDK available.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	common "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	market "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	research "github.com/eighred/kanz/kanz-schemas-go/research/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func field(number protowire.Number, bytes []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, number, protowire.BytesType), bytes)
}

func marshal(message proto.Message) []byte {
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		panic(err)
	}
	return payload
}

func observation(parts ...[]byte) []byte {
	var wire []byte
	for _, part := range parts {
		wire = append(wire, field(5, part)...)
	}
	return wire
}

type fixture struct {
	name string
	wire []byte
	want *research.CalibrationInput
}

func fixtures() []fixture {
	trade := func(id string, price, size *common.Decimal, conditions ...string) *market.Trade {
		return &market.Trade{TradeId: id, Price: price, Size: size, Conditions: conditions}
	}
	withTrade := func(t *market.Trade) *market.MarketDataEvent {
		return &market.MarketDataEvent{InstrumentId: "bond-A", Data: &market.MarketDataEvent_Trade{Trade: t}}
	}
	quote := &market.MarketDataEvent{InstrumentId: "bond-A", Data: &market.MarketDataEvent_Quote{Quote: &market.Quote{}}}
	calibration := func(e *market.MarketDataEvent) *research.CalibrationInput {
		return &research.CalibrationInput{Observation: e}
	}
	price := &common.Decimal{Coefficient: 12345, Exponent: -2}
	size := &common.Decimal{Coefficient: 8}
	base := withTrade(trade("first", price, nil, "A"))
	remainder := trade("", nil, size, "B")
	fragment := field(6, marshal(remainder))
	merged := withTrade(trade("first", price, size, "A", "B"))
	sequenced := proto.Clone(base).(*market.MarketDataEvent)
	sequenced.SourceSequence = 7
	cleared := proto.Clone(base).(*market.MarketDataEvent)
	cleared.GetTrade().TradeId = ""
	return []fixture{
		{"absent", nil, calibration(nil)},
		{"empty_message", observation(nil), calibration(&market.MarketDataEvent{})},
		{"metadata", observation(marshal(base), marshal(&market.MarketDataEvent{SourceSequence: 7})), calibration(sequenced)},
		{"same_variant", observation(marshal(base), fragment), calibration(merged)},
		{"same_variant_single_parent", observation(append(marshal(base), fragment...)), calibration(merged)},
		{"empty_same_variant", observation(marshal(base), field(6, nil)), calibration(base)},
		{"switch", observation(marshal(base), field(7, nil)), calibration(quote)},
		{"switch_back", observation(marshal(base), field(7, nil), fragment), calibration(withTrade(remainder))},
		{"explicit_default", observation(marshal(base), field(6, field(3, nil))), calibration(cleared)},
	}
}

func run(mode, dir string) error {
	for _, test := range fixtures() {
		var observed research.CalibrationInput
		if err := proto.Unmarshal(test.wire, &observed); err != nil || !proto.Equal(&observed, test.want) {
			return fmt.Errorf("%s: Go disagreed with expected value: %v", test.name, err)
		}
		path := filepath.Join(dir, test.name)
		switch mode {
		case "write":
			if err := os.WriteFile(path+".pb", test.wire, 0600); err != nil {
				return err
			}
		case "check":
			bytes, err := os.ReadFile(path + ".decoded")
			if err != nil {
				return err
			}
			var actual research.CalibrationInput
			if err := proto.Unmarshal(bytes, &actual); err != nil || !proto.Equal(&actual, test.want) {
				return fmt.Errorf("%s: Julia round-trip differs from the Go fixture: %v", test.name, err)
			}
		default:
			return fmt.Errorf("unknown fixture operation %q", mode)
		}
		fmt.Println(test.name)
	}
	return nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: wire_oracle write|check FIXTURE_DIRECTORY")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

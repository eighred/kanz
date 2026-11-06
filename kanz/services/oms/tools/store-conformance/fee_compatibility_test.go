package storeconformance

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestPinnedGoUnicodeTable(t *testing.T) {
	cmd := exec.Command("go", "run", "./unicodegen")
	generated, e := cmd.Output()
	if e != nil {
		t.Fatal("pinned Go Unicode generator failed")
	}
	tracked, e := os.ReadFile("../../../../../kanz-rs/crates/oms-store/src/go_unicode.rs")
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(generated, tracked) {
		t.Fatal("Rust normalization table differs from pinned Go Unicode data")
	}
}
func TestRustFeeUnicodeCompatibility(t *testing.T) {
	fixtureBinary(t)
	h := sha256.New()
	var n [8]byte
	var scalar [4]byte
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		v := strings.ToLower(strings.TrimSpace(string(r)))
		binary.BigEndian.PutUint32(scalar[:], uint32(r))
		h.Write(scalar[:])
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		h.Write(n[:])
		h.Write([]byte(v))
	}
	expect(t, run(t, map[string]any{"mode": "fee-unicode"}), hex.EncodeToString(h.Sum(nil)))
	for _, actor := range []string{" Maker ", "\u0130", "\u03a3", "\u1e9e", "\u2003MAKER\u00a0", "\u200bMaker\u200b", "\x00A", "A\u2003B", "", "\n\t"} {
		result := runResult(t, map[string]any{"mode": "fee-subject", "fill": actor, "other": actor})
		want := strings.ToLower(strings.TrimSpace(actor))
		if result.Status != want || string(result.State) != want {
			t.Fatal("actor normalization differs")
		}
	}
}
func varField(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}
func msgField(n protowire.Number, v []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), v)
}
func joined(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
func reversedFields(t *testing.T, raw []byte) []byte {
	t.Helper()
	var fields [][]byte
	for len(raw) > 0 {
		_, _, n := protowire.ConsumeField(raw)
		if n < 0 {
			t.Fatal("invalid independent fixture")
		}
		fields = append(fields, raw[:n])
		raw = raw[n:]
	}
	var result []byte
	for i := len(fields) - 1; i >= 0; i-- {
		result = append(result, fields[i]...)
	}
	return result
}

type feeWireCase struct {
	name  string
	a, b  []byte
	equal bool
}

func feeWireCases(t *testing.T) []feeWireCase {
	t.Helper()
	base := wire(t, fill())
	u := varField(1000, 1)
	v := msgField(1001, []byte("future"))
	second := varField(1000, 2)
	merged := wire(t, &orderpb.Fill{Quantity: &commonpb.Decimal{Coefficient: 1, Exponent: -2}})
	pieces := joined(msgField(5, varField(1, 2)), msgField(5, varField(2, 3)))
	nested := msgField(7, msgField(1, joined(varField(1, 2), u, v)))
	nestedReordered := msgField(7, msgField(1, joined(varField(1, 2), v, u)))
	group := joined(protowire.AppendTag(nil, 1002, protowire.StartGroupType), u, v, protowire.AppendTag(nil, 1002, protowire.EndGroupType))
	groupReordered := joined(protowire.AppendTag(nil, 1002, protowire.StartGroupType), v, u, protowire.AppendTag(nil, 1002, protowire.EndGroupType))
	deepGroup := joined(bytes.Repeat(protowire.AppendTag(nil, 1002, protowire.StartGroupType), 10001), bytes.Repeat(protowire.AppendTag(nil, 1002, protowire.EndGroupType), 10001))
	noncanonicalValue := joined(protowire.AppendTag(nil, 1000, protowire.VarintType), []byte{0x81, 0})
	tag := protowire.AppendTag(nil, 1000, protowire.VarintType)
	tag[len(tag)-1] |= 128
	tag = append(tag, 0)
	return []feeWireCase{
		{"known-field-order", base, reversedFields(t, base), true},
		{"distinct-unknown-order", joined(base, u, v), joined(base, v, u), true},
		{"same-unknown-order", joined(base, u, second), joined(base, second, u), false},
		{"unknown-value-encoding", joined(base, u), joined(base, noncanonicalValue), false},
		{"unknown-tag-normalized", joined(base, u), joined(base, tag, []byte{1}), true},
		{"nested-unknown-order", nested, nestedReordered, true},
		{"nested-unknown-difference", nested, msgField(7, msgField(1, joined(varField(1, 2), second, v))), false},
		{"duplicate-embedded-merge", pieces, merged, true},
		{"duplicate-embedded-unknown-only", joined(msgField(5, u), msgField(5, v)), msgField(5, joined(u, v)), true},
		{"nested-unknown-tag-normalized", msgField(5, u), msgField(5, joined(tag, []byte{1})), true},
		{"duplicate-embedded-unknown", joined(msgField(5, joined(varField(1, 2), u)), msgField(5, joined(varField(2, 3), v))), msgField(5, joined(varField(1, 2), varField(2, 3), u, v)), true},
		{"unknown-group", joined(base, group), joined(base, group), true},
		{"unknown-group-depth-boundary", deepGroup, deepGroup, true},
		{"unknown-group-inner-order", joined(base, group), joined(base, groupReordered), false},
		{"unknown-group-inner-noncanonical-tag", joined(protowire.AppendTag(nil, 1002, protowire.StartGroupType), u, protowire.AppendTag(nil, 1002, protowire.EndGroupType)), joined(protowire.AppendTag(nil, 1002, protowire.StartGroupType), tag, []byte{1}, protowire.AppendTag(nil, 1002, protowire.EndGroupType)), false},
		{"known-enum-wrong-wire", joined(base, msgField(4, []byte{255})), base, false},
		{"known-wrong-wire-is-unknown", joined(base, varField(1, 7)), joined(base, varField(1, 7)), true},
		{"known-wrong-wire-not-dropped", joined(base, varField(1, 7)), base, false},
		{"optional-absent-present", nil, msgField(7, nil), false},
		{"scalar-default", nil, msgField(1, nil), true},
		{"duplicate-scalar-last", joined(msgField(1, []byte("first")), msgField(1, []byte("last"))), msgField(1, []byte("last")), true},
	}
}
func TestFeeWireIndependentSemantics(t *testing.T) {
	for _, tc := range feeWireCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			var a, b orderpb.Fill
			if proto.Unmarshal(tc.a, &a) != nil || proto.Unmarshal(tc.b, &b) != nil || proto.Equal(&a, &b) != tc.equal {
				t.Fatal("independent semantic expectation disagrees with Go")
			}
		})
	}
}
func TestRustFeeWireCompatibility(t *testing.T) {
	fixtureBinary(t)
	for _, tc := range feeWireCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			var a, b orderpb.Fill
			if proto.Unmarshal(tc.a, &a) != nil || proto.Unmarshal(tc.b, &b) != nil {
				t.Fatal("Go refused intended valid fixture")
			}
			if proto.Equal(&a, &b) != tc.equal {
				t.Fatal("independent semantic expectation disagrees with Go")
			}
			result := runResult(t, map[string]any{"mode": "fee-wire", "fill": base64.StdEncoding.EncodeToString(tc.a), "other": base64.StdEncoding.EncodeToString(tc.b)})
			want := "false"
			if tc.equal {
				want = "true"
			}
			expect(t, result.Status, want)
			expected, e := proto.MarshalOptions{Deterministic: true}.Marshal(&a)
			if e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(result.State, expected) {
				t.Fatal("journal deterministic representation differs")
			}
		})
	}
}

func TestRustFeeWireRefusals(t *testing.T) {
	fixtureBinary(t)
	for _, tc := range []struct {
		name   string
		raw    []byte
		status string
	}{
		{"zero-field", []byte{0}, "InvalidInput"},
		{"truncated-varint", []byte{8, 128}, "InvalidInput"},
		{"unterminated-group", []byte{11}, "InvalidInput"},
		{"unexpected-end-group", []byte{12}, "InvalidInput"},
		{"wrong-end-group", []byte{11, 20}, "InvalidInput"},
		{"oversized-input", make([]byte, (1<<20)+1), "Capacity"},
		{"unknown-group-excess-depth", joined(bytes.Repeat([]byte{11}, 10002), bytes.Repeat([]byte{12}, 10002)), "InvalidInput"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runResult(t, map[string]any{"mode": "fee-wire", "fill": base64.StdEncoding.EncodeToString(tc.raw), "other": ""})
			readError(t, r, tc.status)
		})
	}
}

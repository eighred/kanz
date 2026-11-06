package projection

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"math/big"
	"sort"
)

// The checkpoint wire format rounds execution rationals to Decimal. It cannot
// be its own equality oracle: full 1e-10 and restored zero serialize identically.
// Compare existing fold state exactly, then bind it alongside the metadata
// checkpoint digest. This implements no financial arithmetic or second fold.
func compareReplayExact(full, restored *Projection) ([32]byte, error) {
	full.mu.RLock()
	defer full.mu.RUnlock()
	restored.mu.RLock()
	defer restored.mu.RUnlock()
	h := &exactReplayHasher{h: sha256.New()}
	left, right := full.accounts[full.tenant], restored.accounts[restored.tenant]
	if len(left) != len(right) {
		return [32]byte{}, ErrReplayMismatch
	}
	if err := h.count(len(left)); err != nil {
		return [32]byte{}, err
	}
	ids := make([]string, 0, len(left))
	for id := range left {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a, b := left[id], right[id]
		if b == nil || len(a.execs) != len(b.execs) {
			return [32]byte{}, ErrReplayMismatch
		}
		if err := h.part([]byte(id)); err != nil {
			return [32]byte{}, err
		}
		if err := h.count(len(a.execs)); err != nil {
			return [32]byte{}, err
		}
		for i, e := range a.execs {
			other := b.execs[i]
			for _, pair := range [][2]*big.Rat{{e.qty, other.qty}, {e.price, other.price}, {e.fee, other.fee}} {
				if err := h.rational(pair[0], pair[1]); err != nil {
					return [32]byte{}, err
				}
			}
		}
		for _, pair := range [][2]map[string]*posState{{a.baseline, b.baseline}, {a.live, b.live}} {
			if err := h.lots(pair[0], pair[1]); err != nil {
				return [32]byte{}, err
			}
		}
	}
	var digest [32]byte
	copy(digest[:], h.h.Sum(nil))
	return digest, nil
}

type exactReplayHasher struct {
	h     hash.Hash
	bytes int64
}

func (h *exactReplayHasher) part(data []byte) error {
	h.bytes += int64(len(data)) + 8
	if h.bytes > ReplayMaxBytes {
		return ErrReplayLimit
	}
	hashReplayPart(h.h, data)
	return nil
}

func (h *exactReplayHasher) count(n int) error {
	var value [8]byte
	binary.BigEndian.PutUint64(value[:], uint64(n))
	return h.part(value[:])
}

func (h *exactReplayHasher) rational(a, b *big.Rat) error {
	if a == nil || b == nil {
		if a != b {
			return ErrReplayMismatch
		}
		return h.part([]byte{0})
	}
	// Check bit lengths before allocating integer byte encodings or Cmp products.
	for _, r := range []*big.Rat{a, b} {
		if r.Num().BitLen() > 8*int(ReplayMaxFactBytes) || r.Denom().BitLen() > 8*int(ReplayMaxFactBytes) {
			return ErrReplayLimit
		}
	}
	if a.Cmp(b) != 0 {
		return ErrReplayMismatch
	}
	if err := h.part([]byte{1, byte(a.Sign() + 1)}); err != nil {
		return err
	}
	if err := h.part(a.Num().Bytes()); err != nil {
		return err
	}
	return h.part(a.Denom().Bytes())
}

func (h *exactReplayHasher) lots(a, b map[string]*posState) error {
	if len(a) != len(b) {
		return ErrReplayMismatch
	}
	if err := h.count(len(a)); err != nil {
		return err
	}
	keys := make([]string, 0, len(a))
	for key := range a {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		left, right := a[key], b[key]
		if left == nil || right == nil {
			return ErrReplayInput
		}
		if err := h.part([]byte(key)); err != nil {
			return err
		}
		for _, pair := range [][2]*big.Rat{{left.Qty, right.Qty}, {left.AvgCost, right.AvgCost}, {left.Realized, right.Realized}} {
			if err := h.rational(pair[0], pair[1]); err != nil {
				return err
			}
		}
	}
	return nil
}

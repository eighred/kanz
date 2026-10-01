package replay

import (
	"encoding/json"
	"errors"

	"github.com/eighred/kanz/internal/risk/domain"
)

// Repeated market panels and unchanged positions are retained once per tenant.
// References preserve position order and provider lookup keys. The full manifest
// digest remains independent of this storage encoding.
type packedManifest struct {
	Header    manifest
	Inputs    map[string]string
	Positions []string
}

func pack(body []byte) ([]byte, map[string][]byte, error) {
	var m manifest
	if len(body) > maxInputBytes {
		return nil, nil, errors.New("risk manifest exceeds input budget")
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, err
	}
	objects := make(map[string][]byte)
	put := func(b []byte) string { digest := manifestDigest(b); objects[digest] = b; return digest }
	p := packedManifest{Header: m, Inputs: make(map[string]string, len(m.Inputs))}
	for key, value := range m.Inputs {
		p.Inputs[key] = put(value)
	}
	if m.Portfolio.Positions != nil {
		p.Positions = make([]string, 0, len(m.Portfolio.Positions))
	}
	for _, position := range m.Portfolio.Positions {
		b, err := json.Marshal(position)
		if err != nil {
			return nil, nil, err
		}
		p.Positions = append(p.Positions, put(b))
	}
	p.Header.Inputs = nil
	p.Header.Portfolio.Positions = nil
	b, err := json.Marshal(p)
	return b, objects, err
}

func unpack(p packedManifest, objects map[string][]byte) ([]byte, error) {
	m := p.Header
	m.Inputs = make(map[string]json.RawMessage, len(p.Inputs))
	total := 0
	read := func(digest string) ([]byte, error) {
		b, ok := objects[digest]
		if !ok || manifestDigest(b) != digest {
			return nil, errors.New("retained risk input object missing or corrupted")
		}
		total += len(b)
		if total > maxInputBytes {
			return nil, errors.New("expanded risk manifest exceeds input budget")
		}
		return b, nil
	}
	for key, digest := range p.Inputs {
		b, err := read(digest)
		if err != nil {
			return nil, err
		}
		m.Inputs[key] = b
	}
	if p.Positions != nil {
		m.Portfolio.Positions = make([]domain.Position, 0, len(p.Positions))
	}
	for _, digest := range p.Positions {
		b, err := read(digest)
		if err != nil {
			return nil, err
		}
		var position domain.Position
		if err := json.Unmarshal(b, &position); err != nil {
			return nil, err
		}
		m.Portfolio.Positions = append(m.Portfolio.Positions, position)
	}
	return json.Marshal(m)
}

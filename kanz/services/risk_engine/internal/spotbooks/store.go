// Package spotbooks retains at most two current observations per explicitly
// configured spot pair. The tenant-specific risk service owns this read cache.
package spotbooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/instrument"
	"github.com/eighred/kanz/internal/marketedge/ingest"
	"github.com/eighred/kanz/internal/risk/scenario"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const ScenarioName = "OPERATOR_SPOT_BOOKS"
const Subject = ingest.SubjectBookSnapshot

// Grant identifies the feed release and instruments the configured tenant
// may display and export. It carries no venue account or trading authority.
type Grant struct {
	Tenant      string    `json:"tenant"`
	Instrument  string    `json:"instrument"`
	ProductType string    `json:"product_type"`
	Source      string    `json:"source"`
	Version     string    `json:"producer_version"`
	Rights      string    `json:"rights_reference"`
	BaseRef     string    `json:"base_asset_ref"`
	QuoteRef    string    `json:"quote_asset_ref"`
	Until       time.Time `json:"valid_until"`
}

func ParseGrants(raw, tenant string) (map[string]Grant, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var entries []Grant
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 65536 || decoder.Decode(&entries) != nil || decoder.Decode(new(any)) != io.EOF || len(entries) == 0 || len(entries) > 128 {
		return nil, errors.New("spot book grants require a JSON array of 1–128 instruments")
	}
	out := make(map[string]Grant, len(entries))
	for _, g := range entries {
		if g.Tenant == "" || g.Tenant != tenant {
			return nil, errors.New("spot book grant does not belong to the serving tenant")
		}
		p, ok := instrument.ParseID(g.Instrument)
		if !ok || p.Base == p.Quote || g.Instrument != p.Base+"-"+p.Quote || g.ProductType != "CRYPTO_SPOT" || g.BaseRef == g.QuoteRef ||
			g.Version == "unstamped" || g.Version == "dev" || strings.HasSuffix(g.Version, "-dirty") || out[g.Instrument].Instrument != "" {
			return nil, errors.New("invalid or duplicate spot pair, references or producer revision")
		}
		for _, value := range []string{g.Instrument, g.Source, g.Version, g.Rights, g.BaseRef, g.QuoteRef} {
			if len(value) == 0 || len(value) > 128 || strings.TrimSpace(value) != value {
				return nil, errors.New("missing or invalid spot book grant field")
			}
		}
		out[g.Instrument] = g
	}
	return out, nil
}

type bookKey struct{ instrument, venue string }
type Store struct {
	tenant string
	grants map[string]Grant
	mu     sync.RWMutex
	latest map[bookKey]*querypb.ObservedVenueBook
	now    func() time.Time
}

func New(tenant string, grants map[string]Grant) *Store {
	if tenant == "" || len(grants) == 0 {
		return nil
	}
	return &Store{tenant: tenant, grants: maps.Clone(grants), latest: make(map[bookKey]*querypb.ObservedVenueBook), now: time.Now}
}

func (s *Store) Handle(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
	g, ok := s.grants[env.GetPartitionKey()]
	if !ok || env.GetTenantId() != s.tenant || env.Source != g.Source || env.ProducerVersion != g.Version {
		return nil
	}
	now := s.now().UTC()
	if !g.Until.IsZero() && !now.Before(g.Until) {
		return nil
	}
	if env.EventType != Subject || env.Domain != "market" || env.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT ||
		env.SchemaVersion != 1 || env.PayloadSchemaRef != "market.v1.OrderBookSnapshot:1" || env.EventId == "" {
		return errors.New("unexpected spot book fact identity")
	}
	var b marketpb.OrderBookSnapshot
	if err := proto.Unmarshal(payload, &b); err != nil {
		return err
	}
	if b.InstrumentId != g.Instrument || (b.Mic != "OKX" && b.Mic != "BINANCE") {
		return nil
	}
	key := bookKey{b.InstrumentId, b.Mic}
	known := timestamppb.New(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	prior := s.latest[key]
	if b.EventTime.CheckValid() != nil || b.EventTime.AsTime().After(now) {
		if prior != nil {
			prior.EventId = ""
		}
		return nil
	}
	_, inDomain := dec.InDomainDeep(&b)
	validClocks := env.IngestionTime.CheckValid() == nil && env.PublishTime.CheckValid() == nil &&
		!env.IngestionTime.AsTime().Before(b.EventTime.AsTime()) && !env.PublishTime.AsTime().Before(env.IngestionTime.AsTime()) && !now.Before(env.PublishTime.AsTime())
	valid := inDomain && validClocks && len(env.QualityFlags) == 0 && scenario.ValidateObservedBook(&b, known, now) == nil
	if prior != nil {
		old := prior.Book
		// Repeated periodic snapshots must not refresh the receipt time.
		delta := b.EventTime.AsTime().Sub(old.EventTime.AsTime())
		if delta < 0 || (delta == 0 && b.LastUpdateSequence < old.LastUpdateSequence) {
			return nil
		}
		if delta == 0 && b.LastUpdateSequence == old.LastUpdateSequence {
			if !valid || !proto.Equal(old, &b) {
				prior.EventId = ""
			}
			return nil
		}
	}
	// A later impaired or invalid state withdraws the earlier observation.
	// Keep its revision so delayed older deliveries cannot reinstate it.
	next := &querypb.ObservedVenueBook{Book: &b, KnownAt: known}
	if valid {
		next.EventId = env.EventId
	} else {
		next.Book = &marketpb.OrderBookSnapshot{EventTime: b.EventTime, LastUpdateSequence: b.LastUpdateSequence}
	}
	s.latest[key] = next
	return nil
}

func (s *Store) Observe(tenant, id string) *querypb.SpotBookObservation {
	out := &querypb.SpotBookObservation{InstrumentId: id, Status: "UNKNOWN", Reason: "No authorized current pair of books is available."}
	if s == nil || tenant != s.tenant {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.now().UTC()
	g, ok := s.grants[id]
	if !ok || (!g.Until.IsZero() && !now.Before(g.Until)) {
		return out
	}
	var pair []*querypb.ObservedVenueBook
	for _, venue := range []string{"BINANCE", "OKX"} {
		o := s.latest[bookKey{id, venue}]
		if o == nil || o.EventId == "" || scenario.ValidateObservedBook(o.Book, o.KnownAt, now) != nil {
			return out
		}
		pair = append(pair, o)
	}
	if pair[0].Book.EventTime.AsTime().Sub(pair[1].Book.EventTime.AsTime()).Abs() > scenario.SpotBookSkew {
		return out
	}
	out.Status, out.Reason, out.AsOf = "SUPPORTED", "", timestamppb.New(now)
	out.Source, out.ProducerVersion, out.RightsReference = g.Source, g.Version, g.Rights
	out.BaseAssetRef, out.QuoteAssetRef = g.BaseRef, g.QuoteRef
	out.ProductType = g.ProductType
	for _, o := range pair {
		out.Venues = append(out.Venues, proto.CloneOf(o))
	}
	return out
}

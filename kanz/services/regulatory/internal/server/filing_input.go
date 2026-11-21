package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/filing"
	regulatorypublish "github.com/eighred/kanz/internal/regulatory/publish"
	sustainabilitypublish "github.com/eighred/kanz/internal/sustainability/publish"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// filingInput retains only known, decoded inputs and the exact money presented
// at the existing float analytic edge. It contains no headers or opaque JSON.
type filingInput struct {
	manifest []byte
	digest   string
	holdings []sustainabilitypublish.Input
}

func readFilingInput(w http.ResponseWriter, r *http.Request, target any, ratFields []string, climate *disclosureRequest) (filingInput, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body exceeds filing budget"})
		} else {
			badRequest(w, errors.New("invalid request body"))
		}
		return filingInput{}, false
	}
	if err := checkFilingJSON(raw, reflect.TypeOf(target)); err != nil {
		badRequest(w, err)
		return filingInput{}, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		badRequest(w, errors.New("filing input must be an object"))
		return filingInput{}, false
	}
	// math/big accepts a much wider language and exponent than the wire. Check
	// its bounded decimal tokens BEFORE its TextUnmarshaler can allocate limbs.
	for _, field := range ratFields {
		value, present := rawField(object, field)
		var text string
		if !present || json.Unmarshal(value, &text) != nil || len(text) > 128 {
			badRequest(w, errors.New("exact decimal money string required"))
			return filingInput{}, false
		}
		if _, ok := dec.ParseProtoExact(text); !ok {
			badRequest(w, errors.New("money is not exactly representable"))
			return filingInput{}, false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		badRequest(w, errors.New("unknown field or invalid filing input"))
		return filingInput{}, false
	}
	var at **time.Time
	switch request := target.(type) {
	case *frtbRequest:
		at = &request.AsOf
	case *formPFRequest:
		at = &request.AsOf
	case *aifmdRequest:
		at = &request.AsOf
	case *disclosureRequest:
		at = &request.AsOf
	default:
		badRequest(w, errors.New("unsupported filing request"))
		return filingInput{}, false
	}
	if *at != nil {
		canonical := (*at).UTC()
		*at = &canonical
	}
	var inputs []sustainabilitypublish.Input
	if climate != nil {
		if climate.Scenario.CarbonPrice < 0 || climate.Scenario.PhysicalSeverity < 0 || climate.Scenario.PhysicalSeverity > 1 || ((climate.Scenario.CarbonPrice != 0 || climate.Scenario.PhysicalSeverity != 0) && !filing.BoundedIdentifier(climate.Scenario.Name)) {
			badRequest(w, errors.New("explicit bounded climate scenario required"))
			return filingInput{}, false
		}
		rowsRaw, present := rawField(object, "Holdings")
		var rows []map[string]json.RawMessage
		if !present || json.Unmarshal(rowsRaw, &rows) != nil || len(rows) > sustainabilitypublish.MaxHoldings || len(rows) != len(climate.Holdings) {
			badRequest(w, errors.New("bounded holdings inventory required"))
			return filingInput{}, false
		}
		inputs = make([]sustainabilitypublish.Input, 0, len(rows))
		instrumentIDs := make(map[string]bool, len(climate.Holdings))
		for _, holding := range climate.Holdings {
			instrumentIDs[holding.InstrumentID] = true
		}
		for instrument := range climate.FossilFuel {
			if !filing.BoundedIdentifier(instrument) || !instrumentIDs[instrument] {
				badRequest(w, errors.New("fossil-fuel input names an unknown holding"))
				return filingInput{}, false
			}
		}
		for i, row := range rows {
			market, present, err := sourceMoney(row, "MarketValue")
			if err != nil || !present {
				badRequest(w, errors.New("explicit exact holding market value required"))
				return filingInput{}, false
			}
			carbonRaw, present := rawField(row, "Carbon")
			var carbon map[string]json.RawMessage
			if !present || json.Unmarshal(carbonRaw, &carbon) != nil || carbon == nil {
				badRequest(w, errors.New("explicit carbon observation required"))
				return filingInput{}, false
			}
			for _, field := range []string{"Scope1", "Scope2", "Scope3"} {
				if value, present := rawField(carbon, field); !present || bytes.Equal(value, []byte("null")) {
					badRequest(w, errors.New("each observed carbon scope must be explicit"))
					return filingInput{}, false
				}
			}
			revenue, _, rerr := sourceMoney(carbon, "Revenue")
			enterprise, _, eerr := sourceMoney(carbon, "EVIC")
			if rerr != nil || eerr != nil {
				badRequest(w, errors.New("carbon source money is not exactly representable"))
				return filingInput{}, false
			}
			esgRaw, esgPresent := rawField(row, "ESG")
			if esgPresent {
				var esg map[string]json.RawMessage
				if json.Unmarshal(esgRaw, &esg) != nil || esg == nil {
					badRequest(w, errors.New("ESG must be a complete observation or absent"))
					return filingInput{}, false
				}
				for _, field := range []string{"Overall", "Environmental", "Social", "Governance"} {
					if value, present := rawField(esg, field); !present || bytes.Equal(value, []byte("null")) {
						badRequest(w, errors.New("each ESG pillar must be explicit"))
						return filingInput{}, false
					}
				}
			}
			inputs = append(inputs, sustainabilitypublish.Input{Holding: climate.Holdings[i], MarketValue: market, Revenue: revenue, Enterprise: enterprise, ESGPresent: esgPresent})
		}
	}
	// Marshal the typed input, not the original JSON. Exact-money sidecars
	// preserve source precision while the model's float inputs remain explicit.
	manifest, err := json.Marshal(struct {
		Version          int             `json:"version"`
		Origin           string          `json:"origin"`
		Request          any             `json:"request"`
		ExactSourceMoney []sourceAmounts `json:"exact_source_money,omitempty"`
	}{1, "filing-request/unverified", target, moneyManifest(inputs)})
	if err != nil || len(manifest) > regulatorypublish.MaxManifestBytes {
		badRequest(w, errors.New("filing input manifest exceeds its budget"))
		return filingInput{}, false
	}
	digest := sha256.Sum256(manifest)
	return filingInput{manifest: manifest, digest: hex.EncodeToString(digest[:]), holdings: inputs}, true
}

type sourceAmounts struct {
	Instrument  string  `json:"instrument"`
	MarketValue *string `json:"market_value"`
	Revenue     *string `json:"revenue,omitempty"`
	Enterprise  *string `json:"enterprise,omitempty"`
	ESGPresent  bool    `json:"esg_present"`
}

func moneyManifest(inputs []sustainabilitypublish.Input) []sourceAmounts {
	if inputs == nil {
		return nil
	}
	out := make([]sourceAmounts, 0, len(inputs))
	for _, in := range inputs {
		out = append(out, sourceAmounts{in.Holding.InstrumentID, sourceDecimalLiteral(in.MarketValue), sourceDecimalLiteral(in.Revenue), sourceDecimalLiteral(in.Enterprise), in.ESGPresent})
	}
	return out
}

// A lossless source token, not the eight-place filed renderer. Strings avoid
// turning an exact int64 coefficient into a browser JSON-number assumption.
func sourceDecimalLiteral(value *commonpb.Decimal) *string {
	if value == nil {
		return nil
	}
	text := strconv.FormatInt(value.Coefficient, 10) + "e" + strconv.FormatInt(int64(value.Exponent), 10)
	return &text
}

func sourceMoney(object map[string]json.RawMessage, name string) (*commonpb.Decimal, bool, error) {
	raw, present := rawField(object, name)
	if !present {
		return nil, false, nil
	}
	// The existing float field consumes a JSON number. Quoted, null and
	// unbounded tokens cannot silently become a measured zero.
	if len(raw) == 0 || len(raw) > 128 || raw[0] == '"' || bytes.Equal(raw, []byte("null")) {
		return nil, true, errors.New("decimal number required")
	}
	value, ok := dec.ParseProtoExact(string(raw))
	if !ok {
		return nil, true, errors.New("unrepresentable decimal")
	}
	return value, true, nil
}

func rawField(object map[string]json.RawMessage, name string) (json.RawMessage, bool) {
	for key, value := range object {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return nil, false
}

// encoding/json accepts repeated case-folded struct keys and replaces malformed
// surrogate text. Refuse both rather than bind a different input from the one a
// caller thought it supplied. Budgets apply before typed unmarshalling.
func checkFilingJSON(raw []byte, target reflect.Type) error {
	if !utf8.Valid(raw) {
		return errors.New("filing JSON must be UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	var value func(int, reflect.Type) error
	value = func(depth int, want reflect.Type) error {
		nodes++
		if depth > 24 || nodes > 50000 {
			return errors.New("filing JSON shape exceeds budget")
		}
		token, err := d.Token()
		if err != nil {
			return errors.New("invalid filing JSON")
		}
		for want != nil && want.Kind() == reflect.Pointer {
			want = want.Elem()
		}
		switch t := token.(type) {
		case nil:
			return errors.New("filing JSON null is unknown; omit optional inputs instead")
		case json.Delim:
			if t != '{' && t != '[' {
				return errors.New("invalid filing JSON container")
			}
			seen := map[string]bool{}
			fields := filingJSONFields(want)
			for d.More() {
				var child reflect.Type
				if t == '{' {
					keyToken, err := d.Token()
					key, ok := keyToken.(string)
					if err != nil || !ok || len(key) > 256 || strings.ContainsRune(key, utf8.RuneError) {
						return errors.New("invalid filing JSON key")
					}
					fold := key
					if want != nil && want.Kind() == reflect.Struct {
						fold = foldJSONKey(key)
						child = fields[fold]
					}
					if want != nil && want.Kind() == reflect.Map {
						child = want.Elem()
					}
					if seen[fold] {
						return errors.New("duplicate filing JSON field")
					}
					seen[fold] = true
				}
				if t == '[' && want != nil && (want.Kind() == reflect.Slice || want.Kind() == reflect.Array) {
					child = want.Elem()
				}
				if err := value(depth+1, child); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || (t == '{' && end != json.Delim('}')) || (t == '[' && end != json.Delim(']')) {
				return errors.New("invalid filing JSON end")
			}
		case string:
			if len(t) > 4096 || strings.ContainsRune(t, utf8.RuneError) {
				return errors.New("filing JSON string exceeds budget or has invalid Unicode")
			}
		case json.Number:
			if len(t) > 128 {
				return errors.New("filing JSON number exceeds budget")
			}
		}
		return nil
	}
	if err := value(0, target); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing filing JSON input")
	}
	return nil
}

// Only struct field spellings are case folded by encoding/json. Maps such as
// FossilFuel retain case-sensitive instrument keys; do not turn two legitimate
// instrument identities into a duplicate field or a hand-maintained registry.
func filingJSONFields(want reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	if want == nil || want.Kind() != reflect.Struct {
		return fields
	}
	for i := 0; i < want.NumField(); i++ {
		field := want.Field(i)
		if field.PkgPath != "" {
			continue
		}
		tag := strings.Split(field.Tag.Get("json"), ",")[0]
		if tag == "-" {
			continue
		}
		typeOf := field.Type
		for typeOf.Kind() == reflect.Pointer {
			typeOf = typeOf.Elem()
		}
		if tag == "" && field.Anonymous && typeOf.Kind() == reflect.Struct {
			for key, value := range filingJSONFields(typeOf) {
				fields[key] = value
			}
			continue
		}
		if tag == "" {
			tag = field.Name
		}
		fields[foldJSONKey(tag)] = field.Type
	}
	return fields
}

func foldJSONKey(s string) string {
	return strings.Map(func(r rune) rune {
		smallest := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < smallest {
				smallest = next
			}
		}
		return smallest
	}, s)
}

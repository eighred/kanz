package decode_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	commonv1 "github.com/kanz-eng/kanz-schemas-go/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/eighred/kanz/services/lake-sink/internal/decode"
)

// decimalFDS builds the self-contained FileDescriptorSet the registry would
// serve for common.v1.Decimal, exactly as GET /schemas/{ref} returns it.
func decimalFDS(t *testing.T) []byte {
	t.Helper()
	md := (&commonv1.Decimal{}).ProtoReflect().Descriptor()
	fdp := protodesc.ToFileDescriptorProto(md.ParentFile())
	b, err := proto.Marshal(&descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{fdp},
	})
	if err != nil {
		t.Fatalf("marshal FDS: %v", err)
	}
	return b
}

// registry stands in for the EVT-16 resolve surface, counting requests so the
// cache test can assert it is hit once.
func registry(t *testing.T, status int, body []byte) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if status != http.StatusOK {
			http.Error(w, "nope", status)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestDecodeRoundTrip(t *testing.T) {
	srv, _ := registry(t, http.StatusOK, decimalFDS(t))
	dec := decode.NewDecoder(decode.NewHTTPResolver(srv.URL, nil))

	payload, err := proto.Marshal(&commonv1.Decimal{Coefficient: 12345, Exponent: -2})
	if err != nil {
		t.Fatal(err)
	}
	out, err := dec.Decode(context.Background(), "common.v1.Decimal:1", payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("result not json: %v (%s)", err, out)
	}
	// proto field names (UseProtoNames); int64 renders as a string in protojson.
	if obj["coefficient"] != "12345" {
		t.Errorf("coefficient = %v, want 12345", obj["coefficient"])
	}
	if obj["exponent"] != float64(-2) {
		t.Errorf("exponent = %v, want -2", obj["exponent"])
	}
}

func TestDecodeEmptyRefOrPayload(t *testing.T) {
	dec := decode.NewDecoder(decode.NewHTTPResolver("http://unused", nil))
	for _, tc := range []struct {
		name    string
		ref     string
		payload []byte
	}{
		{"empty ref", "", []byte{1}},
		{"empty payload", "common.v1.Decimal:1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := dec.Decode(context.Background(), tc.ref, tc.payload)
			if err != nil || out != nil {
				t.Fatalf("got (%s, %v), want (nil, nil)", out, err)
			}
		})
	}
}

func TestDecodeNilResolverDisabled(t *testing.T) {
	dec := decode.NewDecoder(nil)
	out, err := dec.Decode(context.Background(), "common.v1.Decimal:1", []byte{1, 2, 3})
	if err != nil || out != nil {
		t.Fatalf("got (%s, %v), want (nil, nil) when decode disabled", out, err)
	}
}

func TestResolveTransientVsPermanent(t *testing.T) {
	t.Run("5xx is transient", func(t *testing.T) {
		srv, _ := registry(t, http.StatusServiceUnavailable, nil)
		_, err := decode.NewHTTPResolver(srv.URL, nil).Resolve(context.Background(), "common.v1.Decimal:1")
		var te *decode.TransientError
		if err == nil || !errors.As(err, &te) {
			t.Fatalf("want TransientError, got %v", err)
		}
	})
	t.Run("404 is permanent", func(t *testing.T) {
		srv, _ := registry(t, http.StatusNotFound, nil)
		_, err := decode.NewHTTPResolver(srv.URL, nil).Resolve(context.Background(), "common.v1.Decimal:1")
		var te *decode.TransientError
		if err == nil || errors.As(err, &te) {
			t.Fatalf("want permanent error, got %v", err)
		}
	})
}

func TestCachingResolverHitsOnce(t *testing.T) {
	srv, hits := registry(t, http.StatusOK, decimalFDS(t))
	r := decode.NewCachingResolver(decode.NewHTTPResolver(srv.URL, nil))
	for i := 0; i < 3; i++ {
		if _, err := r.Resolve(context.Background(), "common.v1.Decimal:1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt64(hits); got != 1 {
		t.Errorf("registry hit %d times, want 1 (immutable ⇒ cached)", got)
	}
}

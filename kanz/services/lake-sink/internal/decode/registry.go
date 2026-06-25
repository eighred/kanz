// Package decode resolves payload schemas from the EVT-16 registry and turns raw
// event payloads into JSON, so the lakehouse columns track the published schema
// rather than a compiled-in type — the LAKE-01a schema-evolution seam.
package decode

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Resolver maps a payload_schema_ref ("<schema-id>:<version>", e.g.
// "market.v1.MarketDataEvent:7") to the MessageDescriptor the registry published
// for it. Resolving from the registry — not a compiled-in type — is what makes
// the sink schema-evolution aware: an additively-evolved payload yields new
// columns with no redeploy.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (protoreflect.MessageDescriptor, error)
}

// TransientError marks a resolve failure worth retrying (network, 5xx) versus a
// permanent one (404 unknown ref, malformed descriptor). The consumer NAKs
// transient errors so the bus redelivers, but lands permanent ones raw rather
// than blocking the partition on a poison message.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// HTTPResolver resolves descriptors over the registry's EVT-16c
// GET /schemas/{ref} surface, which returns the raw FileDescriptorSet bytes.
type HTTPResolver struct {
	baseURL string
	client  *http.Client
}

func NewHTTPResolver(baseURL string, client *http.Client) *HTTPResolver {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &HTTPResolver{baseURL: strings.TrimRight(baseURL, "/"), client: client}
}

func (r *HTTPResolver) Resolve(ctx context.Context, ref string) (protoreflect.MessageDescriptor, error) {
	// ref's ':' is RFC-3986-legal in a path segment (EVT-16c), so pass it
	// unencoded — the registry mux reads it back intact via PathValue.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/schemas/"+ref, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, &TransientError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 500:
		return nil, &TransientError{Err: fmt.Errorf("registry %s: %s", ref, resp.Status)}
	default:
		return nil, fmt.Errorf("registry %s: %s", ref, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &TransientError{Err: err}
	}
	return descriptorFromFDS(body, ref)
}

// descriptorFromFDS parses a self-contained FileDescriptorSet (the registry
// stores one per top-level message, EVT-16b) and returns the message named by
// the ref's schema-id.
func descriptorFromFDS(b []byte, ref string) (protoreflect.MessageDescriptor, error) {
	i := strings.LastIndex(ref, ":")
	if i < 0 {
		return nil, fmt.Errorf("bad schema ref %q", ref)
	}
	schemaID := ref[:i]

	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(b, &fds); err != nil {
		return nil, fmt.Errorf("ref %s: decode FileDescriptorSet: %w", ref, err)
	}
	files, err := protodesc.NewFiles(&fds)
	if err != nil {
		return nil, fmt.Errorf("ref %s: build descriptors: %w", ref, err)
	}
	d, err := files.FindDescriptorByName(protoreflect.FullName(schemaID))
	if err != nil {
		return nil, fmt.Errorf("ref %s: message %s not in descriptor: %w", ref, schemaID, err)
	}
	md, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("ref %s: %s is not a message", ref, schemaID)
	}
	return md, nil
}

// CachingResolver memoizes descriptors per ref. (schema_id, version) is
// immutable once published (EVT-16c ships them `Cache-Control: immutable`), so
// the cache never needs invalidation.
type CachingResolver struct {
	inner Resolver
	mu    sync.RWMutex
	cache map[string]protoreflect.MessageDescriptor
}

func NewCachingResolver(inner Resolver) *CachingResolver {
	return &CachingResolver{inner: inner, cache: map[string]protoreflect.MessageDescriptor{}}
}

func (c *CachingResolver) Resolve(ctx context.Context, ref string) (protoreflect.MessageDescriptor, error) {
	c.mu.RLock()
	md, ok := c.cache[ref]
	c.mu.RUnlock()
	if ok {
		return md, nil
	}
	md, err := c.inner.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.cache[ref] = md
	c.mu.Unlock()
	return md, nil
}

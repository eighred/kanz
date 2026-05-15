// registry-ingest is the kanz-schemas release-time client for the
// schema-registry service (EVT-16). On a kanz-schemas tag push the
// schema-release workflow invokes this tool with the global
// FileDescriptorSet emitted by `buf build`; for each top-level message in
// each proto file the tool builds a per-message subset FDS (file + transitive
// imports) and POSTs it to the registry. The registry assigns the version
// and returns the resulting payload_schema_ref.
//
// Re-runs are safe: the registry skips inserts whose fingerprint matches the
// latest stored version for the schema-id (EVT-16b idempotency contract).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func main() {
	var (
		fdsPath     = flag.String("fds", "", "path to global FileDescriptorSet emitted by `buf build`")
		registryURL = flag.String("registry", "", "schema-registry base URL")
		sourceTag   = flag.String("tag", "", "kanz-schemas release tag")
	)
	flag.Parse()
	if *fdsPath == "" || *registryURL == "" || *sourceTag == "" {
		flag.Usage()
		log.Fatal("-fds, -registry and -tag are required")
	}

	raw, err := os.ReadFile(*fdsPath)
	if err != nil {
		log.Fatalf("read fds: %v", err)
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &fds); err != nil {
		log.Fatalf("unmarshal fds: %v", err)
	}

	byName := make(map[string]*descriptorpb.FileDescriptorProto, len(fds.File))
	for _, f := range fds.File {
		byName[f.GetName()] = f
	}

	client := &http.Client{Timeout: 30 * time.Second}
	baseURL := strings.TrimRight(*registryURL, "/")
	ctx := context.Background()

	var registered, unchanged int
	for _, f := range fds.File {
		pkg := f.GetPackage()
		subsetBytes, err := marshalSubset(f, byName)
		if err != nil {
			log.Fatalf("build subset for %s: %v", f.GetName(), err)
		}
		for _, m := range f.MessageType {
			schemaID := pkg + "." + m.GetName()
			ref, created, err := register(ctx, client, baseURL, schemaID, subsetBytes, *sourceTag)
			if err != nil {
				log.Fatalf("register %s: %v", schemaID, err)
			}
			if created {
				registered++
				log.Printf("registered %s", ref)
			} else {
				unchanged++
				log.Printf("unchanged  %s", ref)
			}
		}
	}
	log.Printf("done: %d registered, %d unchanged", registered, unchanged)
}

// marshalSubset emits a FileDescriptorSet containing root + every transitive
// import. The registry stores this opaque blob; consumers decode it the same
// way `protoc --descriptor_set_in` would.
func marshalSubset(root *descriptorpb.FileDescriptorProto, byName map[string]*descriptorpb.FileDescriptorProto) ([]byte, error) {
	seen := make(map[string]bool)
	var files []*descriptorpb.FileDescriptorProto
	var walk func(*descriptorpb.FileDescriptorProto) error
	walk = func(f *descriptorpb.FileDescriptorProto) error {
		if seen[f.GetName()] {
			return nil
		}
		seen[f.GetName()] = true
		for _, dep := range f.Dependency {
			d, ok := byName[dep]
			if !ok {
				return fmt.Errorf("missing dependency %q for %q", dep, f.GetName())
			}
			if err := walk(d); err != nil {
				return err
			}
		}
		files = append(files, f)
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return proto.Marshal(&descriptorpb.FileDescriptorSet{File: files})
}

type registerResp struct {
	Ref     string `json:"ref"`
	Created bool   `json:"created"`
}

func register(ctx context.Context, c *http.Client, baseURL, schemaID string, descriptor []byte, sourceTag string) (string, bool, error) {
	url := baseURL + "/schemas/" + schemaID
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(descriptor))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Source-Tag", sourceTag)
	resp, err := c.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", false, fmt.Errorf("registry returned %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var r registerResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", false, fmt.Errorf("decode response: %w", err)
	}
	return r.Ref, r.Created, nil
}

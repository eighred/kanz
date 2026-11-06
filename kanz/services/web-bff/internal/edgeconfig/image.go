package edgeconfig

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"regexp"
)

//go:embed image.json
var imageDocument []byte

// ImageIdentity records distinct OCI identity kinds. The config digest is not
// interchangeable with an index/manifest digest when validating scanner output.
type ImageIdentity struct {
	Reference string `json:"reference"`
	Version   string `json:"version"`
	Platform  string `json:"platform"`
	Index     string `json:"index"`
	Manifest  string `json:"manifest"`
	Config    string `json:"config"`
}

var pinnedImage = func() ImageIdentity {
	var value ImageIdentity
	d := json.NewDecoder(bytes.NewReader(imageDocument))
	d.DisallowUnknownFields()
	if d.Decode(&value) != nil || value.Platform != "linux/amd64" ||
		!regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(value.Version) ||
		value.Reference != "envoyproxy/envoy:distroless-v"+value.Version+"@"+value.Index {
		panic("invalid embedded edge image identity")
	}
	for _, digest := range []string{value.Index, value.Manifest, value.Config} {
		if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(digest) {
			panic("invalid embedded edge image digest")
		}
	}
	return value
}()

func Image() string { return pinnedImage.Reference }

// LockedImage returns a copy, so consumers cannot change the shared identity.
func LockedImage() ImageIdentity { return pinnedImage }

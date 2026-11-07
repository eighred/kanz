package snapshot

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"testing"
	"time"
)

func snapshotLandingArchive() []byte {
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	if w.WriteHeader(&tar.Header{Name: "landing/", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 65532, Gid: 65532, Format: tar.FormatUSTAR}) != nil || w.Close() != nil {
		panic("fixed landing archive cannot be encoded")
	}
	return out.Bytes()
}

// Inspect only; never extract a Docker archive onto the host.
func snapshotLandingReadback(raw []byte) bool {
	if len(raw) < 1536 || len(raw) > 64<<10 || len(raw)%512 != 0 || !bytes.Equal(raw[len(raw)-1024:], make([]byte, 1024)) {
		return false
	}
	input := bytes.NewReader(raw)
	reader := tar.NewReader(input)
	h, err := reader.Next()
	if err != nil || h.Name != "landing/" || h.Typeflag != tar.TypeDir || h.Mode != 0o700 || h.Uid != 65532 || h.Gid != 65532 || h.Size != 0 || h.Linkname != "" || len(h.PAXRecords) != 0 || len(h.Xattrs) != 0 {
		return false
	}
	if _, err := reader.Next(); err != io.EOF {
		return false
	}
	remainder, err := io.ReadAll(input)
	return err == nil && bytes.Equal(remainder, make([]byte, len(remainder)))
}

func snapshotLandingTarget(raw []byte, id, nonce, run, volume string) bool {
	if len(raw) == 0 || len(raw) > 4096 || !snapshotLandingUniqueObject(raw, 0) {
		return false
	}
	var v struct {
		ID       string `json:"id"`
		Nonce    string `json:"nonce"`
		Run      string `json:"run"`
		Running  *bool  `json:"running"`
		Status   string `json:"status"`
		Readonly *bool  `json:"readonly"`
		Mount    struct {
			Name        string `json:"name"`
			Type        string `json:"type"`
			Destination string `json:"destination"`
			RW          *bool  `json:"rw"`
		} `json:"mount"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(&v) == nil && d.Decode(new(any)) == io.EOF && v.ID == id && v.Nonce == nonce && v.Run == run && v.Running != nil && !*v.Running && v.Status == "created" && v.Readonly != nil && *v.Readonly && v.Mount.Name == volume && v.Mount.Type == "volume" && v.Mount.Destination == "/tmp" && v.Mount.RW != nil && *v.Mount.RW
}

// The typed decoder rejects unknown fields; this precheck also rejects duplicate
// keys at both allowed object levels, rather than accepting a last-value winner.
func snapshotLandingUniqueObject(raw []byte, depth int) bool {
	if depth > 1 {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return false
		}
		seen[key] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return false
		}
		value = bytes.TrimSpace(value)
		if len(value) == 0 || value[0] == '[' {
			return false
		}
		if value[0] == '{' && !snapshotLandingUniqueObject(value, depth+1) {
			return false
		}
	}
	token, err = d.Token()
	return err == nil && token == json.Delim('}') && d.Decode(new(any)) == io.EOF
}

func TestSnapshotImageLandingProvisioningArchive(t *testing.T) {
	if !snapshotLandingReadback(snapshotLandingArchive()) {
		t.Fatal("fixed landing archive rejected")
	}
	for _, change := range []func(*tar.Header){
		func(h *tar.Header) { h.Name = "other/" }, func(h *tar.Header) { h.Uid = 0 }, func(h *tar.Header) { h.Gid = 0 }, func(h *tar.Header) { h.Mode = 0o777 },
		func(h *tar.Header) { h.Typeflag = tar.TypeReg; h.Name = "landing" }, func(h *tar.Header) { h.Typeflag = tar.TypeSymlink; h.Name = "landing"; h.Linkname = "/tmp" },
		func(h *tar.Header) {
			h.Format = tar.FormatPAX
			h.PAXRecords = map[string]string{"comment": "unexpected"}
		},
	} {
		var out bytes.Buffer
		w := tar.NewWriter(&out)
		h := tar.Header{Name: "landing/", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 65532, Gid: 65532}
		change(&h)
		if w.WriteHeader(&h) != nil || w.Close() != nil {
			t.Fatal("negative archive setup failed")
		}
		if snapshotLandingReadback(out.Bytes()) {
			t.Fatal("unexpected landing metadata accepted")
		}
	}
	var extra bytes.Buffer
	w := tar.NewWriter(&extra)
	for range 2 {
		if w.WriteHeader(&tar.Header{Name: "landing/", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 65532, Gid: 65532}) != nil {
			t.Fatal("extra entry setup failed")
		}
	}
	_ = w.Close()
	for _, raw := range [][]byte{extra.Bytes(), snapshotLandingArchive()[:512], bytes.Repeat([]byte{0}, (64<<10)+512), append(snapshotLandingArchive(), append([]byte{1}, make([]byte, 511)...)...)} {
		if snapshotLandingReadback(raw) {
			t.Fatal("extra/truncated/oversized archive accepted")
		}
	}
	// Moby go-archive chooses PAX format but normalizes timestamps to seconds
	// and omits access/change times; this short fixed name needs no PAX records.
	var daemon bytes.Buffer
	dw := tar.NewWriter(&daemon)
	if dw.WriteHeader(&tar.Header{Name: "landing/", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 65532, Gid: 65532, Format: tar.FormatPAX, ModTime: time.Unix(1700000000, 0)}) != nil || dw.Close() != nil || !snapshotLandingReadback(daemon.Bytes()) {
		t.Fatal("daemon-shaped directory metadata rejected")
	}
	valid := []byte(`{"id":"owned","nonce":"nonce","run":"run","running":false,"status":"created","readonly":true,"mount":{"name":"volume","type":"volume","destination":"/tmp","rw":true}}`)
	if !snapshotLandingTarget(valid, "owned", "nonce", "run", "volume") {
		t.Fatal("owned stopped target refused")
	}
	for _, pair := range [][2]string{{`"owned"`, `"foreign"`}, {`"nonce":"nonce"`, `"nonce":"foreign"`}, {`"run":"run"`, `"run":"foreign"`}, {`"running":false`, `"running":true`}, {`"created"`, `"exited"`}, {`"readonly":true`, `"readonly":false`}, {`"name":"volume"`, `"name":"other"`}, {`"type":"volume"`, `"type":"bind"`}, {`"/tmp"`, `"/other"`}, {`"rw":true`, `"rw":false`}} {
		if snapshotLandingTarget(bytes.Replace(valid, []byte(pair[0]), []byte(pair[1]), 1), "owned", "nonce", "run", "volume") {
			t.Fatal("foreign or active provisioning target accepted")
		}
	}
	for _, raw := range [][]byte{
		bytes.Replace(valid, []byte(`"running":false,`), nil, 1),
		bytes.Replace(valid, []byte(`"running":false`), []byte(`"running":null`), 1),
		bytes.Replace(valid, []byte(`"mount":{`), []byte(`"unknown":true,"mount":{`), 1),
		bytes.Replace(valid, []byte(`"id":"owned"`), []byte(`"id":"foreign","id":"owned"`), 1),
		bytes.Replace(valid, []byte(`"rw":true`), []byte(`"rw":false,"rw":true`), 1),
		append(append([]byte{}, valid...), []byte(`{}`)...),
		[]byte(`{"id":"owned","nonce":"nonce","run":"run","running":false,"status":"created","readonly":true,"mount":null}`),
	} {
		if snapshotLandingTarget(raw, "owned", "nonce", "run", "volume") {
			t.Fatal("ambiguous or incomplete target accepted")
		}
	}

}

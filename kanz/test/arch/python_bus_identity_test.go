package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE PYTHON SERVICE CAN AUTHENTICATE TO THE BROKER (SEC-M3, #241).
//
// Every guard that checks a service has a broker account keys on
// servicesWithEntrypoints — Go packages under kanz/services. The inference
// service is Python, lives in kanz-py/, and was therefore invisible to all of
// them: it had no entry in tenancy.yaml at all, and nothing said so.
//
// That is not a cosmetic gap. infra/nats/nats.yaml sets `verify: true` +
// `verify_and_map`, so a client with no admitted SVID is refused at the TLS
// HANDSHAKE — not denied per-subject, refused outright. And the streaming path is
// the WIRED half: risk-engine publishes inference.feature.computed, and this
// service is what scores it. On a real broker, nothing would.
//
// The check has two halves because there are two ways to be silently broken:
//
//  1. NO ACCOUNT — the service cannot connect at all.
//  2. THE CERT DIRECTORY DISAGREES WITH ITSELF. spiffe-helper writes the SVID
//     where its ConfigMap says, the container mounts a volume somewhere, and the
//     client reads whatever KANZ_INFERENCE_NATS_CERT_DIR names. Three independent
//     strings that must be equal. When they are not, the pod starts, passes its
//     probes, and cannot connect — and the manifest looks right in review because
//     each line is individually plausible.
const inferenceSVID = "spiffe://kanz.internal/ns/kanz-services/sa/inference"

// pythonDialsNATS reports whether kanz-py still opens a broker connection. If it
// ever stops, this guard should be deleted rather than left passing vacuously.
func pythonDialsNATS(t *testing.T, pyRoot string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pyRoot, "kanz_bus", "nats.py"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "nats.connect(")
}

func TestThePythonServiceHasABrokerAccount(t *testing.T) {
	root := moduleRoot(t)
	pyRoot := filepath.Join(filepath.Dir(root), "kanz-py")

	if !pythonDialsNATS(t, pyRoot) {
		t.Skip("kanz-py no longer dials NATS — delete this guard rather than leaving it green")
	}

	pub := servicePublishPermissions(t, filepath.Join(root, "infra", "nats", "tenancy.yaml"))
	sub := serviceSubscribeAllow(t, filepath.Join(root, "infra", "nats", "tenancy.yaml"))

	perm, ok := pub[inferenceSVID]
	if !ok {
		t.Fatalf("tenancy.yaml has no permissions block for %s.\n\n"+
			"kanz-py dials the broker, and nats.yaml sets verify: true + verify_and_map — so with "+
			"no admitted SVID this service is refused at the TLS handshake and scores nothing. "+
			"risk-engine really does publish inference.feature.computed; the streaming path is wired.",
			inferenceSVID)
	}

	// Derived from the Python constants, not restated here — a rename that this
	// guard could not see would be exactly the drift it exists to catch.
	for _, c := range []struct {
		file, constName, direction string
	}{
		{filepath.Join(pyRoot, "kanz_inference", "publish.py"), "SUBJECT_PREDICTION_SCORED", "publish"},
		{filepath.Join(pyRoot, "kanz_inference", "streaming", "worker.py"), "SUBJECT_FEATURE_COMPUTED", "subscribe"},
	} {
		subject := pythonConst(t, c.file, c.constName)
		allow := perm
		if c.direction == "subscribe" {
			allow = sub[inferenceSVID]
		}
		if permDenies(subject, allow) || !covered(subject, allow.allow) {
			t.Errorf("kanz-py %ss %q (%s in %s) but tenancy.yaml's permissions.%s for %s does not "+
				"allow it — the service authenticates fine and is then DENIED on that subject, "+
				"which reads as a broken feature rather than a missing grant",
				c.direction, subject, c.constName, filepath.Base(c.file), c.direction, inferenceSVID)
		}
	}
}

var pyConstRe = regexp.MustCompile(`(?m)^([A-Z_][A-Z0-9_]*)\s*=\s*"([^"]+)"`)

// pythonConst reads a module-level string constant out of a .py file. Text, not
// AST, because the shape is fixed and a Python parser in a Go test would be a
// second thing to maintain — but it FAILS when the constant is absent rather than
// returning "", so a rename cannot quietly empty the assertion.
func pythonConst(t *testing.T, path, name string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, m := range pyConstRe.FindAllStringSubmatch(string(b), -1) {
		if m[1] == name {
			return m[2]
		}
	}
	t.Fatalf("%s does not define %s — it was renamed or moved, and this guard is now blind to "+
		"whatever subject replaced it", path, name)
	return ""
}

// TestTheInferenceSVIDPathsAgree pins the three independent strings that must be
// equal for the SVID to be readable at all.
func TestTheInferenceSVIDPathsAgree(t *testing.T) {
	root := moduleRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "infra", "deploy", "inference-deploy.yaml"))
	if err != nil {
		t.Fatalf("read inference-deploy.yaml: %v", err)
	}

	type container struct {
		Name string `yaml:"name"`
		Env  []struct {
			Name  string `yaml:"name"`
			Value string `yaml:"value"`
		} `yaml:"env"`
		VolumeMounts []struct {
			Name      string `yaml:"name"`
			MountPath string `yaml:"mountPath"`
		} `yaml:"volumeMounts"`
	}
	type doc struct {
		Kind string            `yaml:"kind"`
		Data map[string]string `yaml:"data"`
		Spec struct {
			Template struct {
				Spec struct {
					Containers     []container `yaml:"containers"`
					InitContainers []container `yaml:"initContainers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}

	var helperDir, envDir, mountDir string
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var d doc
		if err := dec.Decode(&d); err != nil {
			break
		}
		if d.Kind == "ConfigMap" {
			if conf, ok := d.Data["helper.conf"]; ok {
				helperDir = confValue(conf, "cert_dir")
			}
		}
		for _, c := range append(append([]container{}, d.Spec.Template.Spec.InitContainers...),
			d.Spec.Template.Spec.Containers...) {
			for _, e := range c.Env {
				if e.Name == "KANZ_INFERENCE_NATS_CERT_DIR" {
					envDir = e.Value
					for _, m := range c.VolumeMounts {
						if m.Name == "inference-certs" {
							mountDir = m.MountPath
						}
					}
				}
			}
		}
	}

	// NON-VACUITY: all three must have been found, or the comparison below is
	// three empty strings agreeing with each other.
	for _, f := range []struct{ what, got string }{
		{"spiffe-helper's cert_dir", helperDir},
		{"KANZ_INFERENCE_NATS_CERT_DIR", envDir},
		{"the inference-certs mountPath on the container that reads it", mountDir},
	} {
		if f.got == "" {
			t.Fatalf("could not find %s in inference-deploy.yaml — the manifest was restructured "+
				"and this guard is comparing nothing", f.what)
		}
	}

	if helperDir != envDir || envDir != mountDir {
		t.Errorf("the SVID directory disagrees with itself:\n"+
			"  spiffe-helper writes to      %s\n"+
			"  the container mounts it at   %s\n"+
			"  the client reads             %s\n\n"+
			"The pod starts, passes its probes, and cannot connect to the broker — because the "+
			"client finds no SVID where it looks and refuses (kanz_bus.mtls.MissingSVIDError), or "+
			"finds nothing and connects with no identity. Each line is individually plausible in "+
			"review, which is why this is checked rather than read.",
			helperDir, mountDir, envDir)
	}
}

// confValue reads `key = "value"` out of a spiffe-helper HCL-ish config.
func confValue(conf, key string) string {
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, key) {
			continue
		}
		_, rest, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		return strings.Trim(strings.TrimSpace(rest), `"`)
	}
	return ""
}

package arch

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	vaultImage        = "hashicorp/vault:1.21.4@sha256:6c77f568e6b6310d5bc68befb5711b9215c574de7da489e7c24332581176888b"
	spiffeHelperImage = "ghcr.io/spiffe/spiffe-helper:0.11.0@sha256:2759b3a699bb63b91cc5896f46cd6f70b9e3dfed9f7f4355a3a0a4e702984f9c"
)

// TestVaultCSIUsesSupportedBoundedWorkloadAuth keeps the secret file boundary
// executable on Vault OSS. The Vault CSI provider authenticates the requesting
// pod with an audience-bound Kubernetes ServiceAccount token; it does not fetch
// a SPIFFE JWT-SVID and it ignores the non-existent `role` parameter.
func TestVaultCSIUsesSupportedBoundedWorkloadAuth(t *testing.T) {
	root := moduleRoot(t)
	paths := []string{
		filepath.Join(root, "infra", "security", "secrets", "secretproviderclass.yaml"),
		filepath.Join(root, "infra", "security", "secrets", "spire-upstream.yaml"),
	}

	found := 0
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		for {
			var doc struct {
				Kind string `yaml:"kind"`
				Spec struct {
					Provider      string            `yaml:"provider"`
					Parameters    map[string]string `yaml:"parameters"`
					SecretObjects any               `yaml:"secretObjects"`
				} `yaml:"spec"`
			}
			if err := dec.Decode(&doc); err != nil {
				if err == io.EOF {
					break
				}
				t.Fatalf("decode %s: %v", path, err)
			}
			if doc.Kind != "SecretProviderClass" || doc.Spec.Provider != "vault" {
				continue
			}
			found++
			p := doc.Spec.Parameters
			if p["roleName"] == "" {
				t.Errorf("%s: Vault SecretProviderClass has no roleName", path)
			}
			if _, unsupported := p["role"]; unsupported {
				t.Errorf("%s: Vault CSI ignores the unsupported role parameter; use roleName", path)
			}
			if p["vaultAuthMountPath"] != "kubernetes" {
				t.Errorf("%s: auth mount = %q, want supported Vault OSS kubernetes auth", path, p["vaultAuthMountPath"])
			}
			if p["audience"] != "vault" {
				t.Errorf("%s: audience = %q, want the dedicated vault audience", path, p["audience"])
			}
			if p["vaultCACertPath"] != "/run/spire/bundle/bundle.crt" {
				t.Errorf("%s: Vault TLS CA path = %q, want the mounted SPIRE bundle", path, p["vaultCACertPath"])
			}
			if p["vaultSkipTLSVerify"] == "true" {
				t.Errorf("%s: Vault TLS verification is disabled", path)
			}
			if doc.Spec.SecretObjects != nil {
				t.Errorf("%s: secretObjects syncs plaintext into a Kubernetes Secret", path)
			}
		}
	}
	if found < 2 {
		t.Fatalf("found only %d Vault SecretProviderClasses; scanner or estate is incomplete", found)
	}
}

func TestVaultCSIProviderCanReadTheConfiguredTrustBundle(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "infra", "security", "secrets", "csi.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name         string `yaml:"name"`
							VolumeMounts []struct {
								Name      string `yaml:"name"`
								MountPath string `yaml:"mountPath"`
							} `yaml:"volumeMounts"`
						} `yaml:"containers"`
						Volumes []struct {
							Name      string `yaml:"name"`
							ConfigMap struct {
								Name string `yaml:"name"`
							} `yaml:"configMap"`
						} `yaml:"volumes"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if doc.Kind != "DaemonSet" || doc.Metadata.Name != "csi-secrets-store" {
			continue
		}
		if doc.Metadata.Namespace != "spire-system" {
			t.Fatalf("CSI provider runs in %q; it must share the rotating spire-bundle ConfigMap namespace", doc.Metadata.Namespace)
		}
		bundleVolume := false
		for _, v := range doc.Spec.Template.Spec.Volumes {
			bundleVolume = bundleVolume || v.Name == "spire-bundle" && v.ConfigMap.Name == "spire-bundle"
		}
		bundleMount := false
		for _, c := range doc.Spec.Template.Spec.Containers {
			if c.Name != "vault-provider" {
				continue
			}
			for _, m := range c.VolumeMounts {
				bundleMount = bundleMount || m.Name == "spire-bundle" && m.MountPath == "/run/spire/bundle"
			}
		}
		if !bundleVolume || !bundleMount {
			t.Fatal("Vault CSI provider cannot read the SPIRE trust bundle configured by every SecretProviderClass")
		}
		return
	}
	t.Fatal("csi-secrets-store DaemonSet not found")
}

func TestVaultCanReviewOnlyWorkloadTokens(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "infra", "security", "secrets", "vault.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			RoleRef struct {
				Kind string `yaml:"kind"`
				Name string `yaml:"name"`
			} `yaml:"roleRef"`
			Subjects []struct {
				Kind      string `yaml:"kind"`
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"subjects"`
		}
		if err := dec.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if doc.Kind != "ClusterRoleBinding" || doc.Metadata.Name != "vault-tokenreview" {
			continue
		}
		if doc.RoleRef.Kind != "ClusterRole" || doc.RoleRef.Name != "system:auth-delegator" || len(doc.Subjects) != 1 {
			t.Fatal("Vault TokenReview binding must reference only the standard system:auth-delegator role")
		}
		s := doc.Subjects[0]
		if s.Kind != "ServiceAccount" || s.Name != "vault" || s.Namespace != "vault" {
			t.Fatalf("Vault TokenReview subject = %#v, want only vault/vault ServiceAccount", s)
		}
		return
	}
	t.Fatal("vault-tokenreview ClusterRoleBinding not found")
}

func TestVaultHasBoundedIMDSAndDeterministicCertificateReload(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "infra", "security", "secrets", "vault.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	type container struct {
		Name  string   `yaml:"name"`
		Image string   `yaml:"image"`
		Args  []string `yaml:"args"`
		Env   []struct {
			Name  string `yaml:"name"`
			Value string `yaml:"value"`
		} `yaml:"env"`
		VolumeMounts []struct {
			Name      string `yaml:"name"`
			MountPath string `yaml:"mountPath"`
		} `yaml:"volumeMounts"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var vaultConfig, helperConfig, helperInitConfig string
	foundStatefulSet := false
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Data map[string]string `yaml:"data"`
			Spec struct {
				Template struct {
					Spec struct {
						HostNetwork           bool        `yaml:"hostNetwork"`
						DNSPolicy             string      `yaml:"dnsPolicy"`
						ShareProcessNamespace bool        `yaml:"shareProcessNamespace"`
						InitContainers        []container `yaml:"initContainers"`
						Containers            []container `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		switch {
		case doc.Kind == "ConfigMap" && doc.Metadata.Name == "vault-config":
			vaultConfig = doc.Data["vault.hcl"]
		case doc.Kind == "ConfigMap" && doc.Metadata.Name == "vault-spiffe-helper":
			helperConfig = doc.Data["helper.conf"]
			helperInitConfig = doc.Data["helper-init.conf"]
		case doc.Kind == "StatefulSet" && doc.Metadata.Name == "vault":
			foundStatefulSet = true
			spec := doc.Spec.Template.Spec
			if !spec.HostNetwork || spec.DNSPolicy != "ClusterFirstWithHostNet" {
				t.Error("Vault must be the sole host-networked workload that reaches IMDSv2 with node hop_limit=1")
			}
			if !spec.ShareProcessNamespace {
				t.Error("spiffe-helper cannot signal the Vault PID without a shared process namespace")
			}
			for _, c := range append(spec.InitContainers, spec.Containers...) {
				switch c.Name {
				case "vault":
					if c.Image != vaultImage {
						t.Errorf("Vault image = %q, want reviewed digest", c.Image)
					}
					if !hasEnv(c.Env, "AWS_REGION", "ap-northeast-1") || !hasMount(c.VolumeMounts, "vault-run", "/vault/run") {
						t.Error("Vault lacks its explicit AWS region or shared PID directory")
					}
					if len(c.Args) != 1 || c.Args[0] != "server" {
						t.Errorf("Vault args = %q; the official entrypoint owns -config and duplicate flags load seals twice", c.Args)
					}
				case "spiffe-helper", "spiffe-helper-init":
					if c.Image != spiffeHelperImage {
						t.Errorf("%s image = %q, want reviewed digest", c.Name, c.Image)
					}
				}
			}
		}
	}
	if !foundStatefulSet {
		t.Fatal("Vault StatefulSet not found")
	}
	for _, want := range []string{
		`pid_file = "/vault/run/vault.pid"`,
		`seal "awskms"`,
		`kms_key_id = "alias/kanz-vault-unseal"`,
		`tls_cert_file = "/run/spire/certs/tls.crt"`,
	} {
		if !bytes.Contains([]byte(vaultConfig), []byte(want)) {
			t.Errorf("Vault config lacks %q", want)
		}
	}
	if !bytes.Contains(b, []byte(`/v1/sys/health?standbyok=true&sealedcode=204&uninitcode=204`)) ||
		!bytes.Contains(b, []byte(`/v1/sys/health?standbyok=true&sealedcode=204`)) {
		t.Error("Vault probes must keep sealed/uninitialized processes live while excluding uninitialized state from readiness")
	}
	if !bytes.Contains([]byte(helperConfig), []byte(`pid_file_name = "/vault/run/vault.pid"`)) ||
		!bytes.Contains([]byte(helperConfig), []byte(`renew_signal  = "SIGHUP"`)) ||
		!bytes.Contains([]byte(helperConfig), []byte(`agent_address = "/run/spire/socket/spire-agent.sock"`)) {
		t.Error("spiffe-helper does not deterministically reload Vault through its PID file")
	}
	if bytes.Contains([]byte(helperConfig), []byte("-HUP 1")) {
		t.Error("spiffe-helper must not signal the Kubernetes sandbox PID")
	}
	if !bytes.Contains([]byte(helperInitConfig), []byte(`agent_address = "/run/spire/socket/spire-agent.sock"`)) ||
		!bytes.Contains([]byte(helperInitConfig), []byte("key_file_mode         = 0600")) ||
		bytes.Contains([]byte(helperInitConfig), []byte("pid_file_name")) ||
		bytes.Contains([]byte(helperInitConfig), []byte("renew_signal")) {
		t.Error("one-shot spiffe-helper config must fetch the SVID without daemon-only signal settings")
	}
	if !bytes.Contains([]byte(helperConfig), []byte("key_file_mode         = 0600")) {
		t.Error("Vault's private key must be readable only by the shared non-root UID")
	}
}

func hasEnv(env []struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}, name, value string) bool {
	for _, e := range env {
		if e.Name == name && e.Value == value {
			return true
		}
	}
	return false
}

func hasMount(mounts []struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
}, name, path string) bool {
	for _, m := range mounts {
		if m.Name == name && m.MountPath == path {
			return true
		}
	}
	return false
}

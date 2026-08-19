package arch

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A STATIC SHARD MEMBER LIST AND AN AUTOSCALER CANNOT BOTH BE RIGHT (#110).
//
// services/risk-engine/internal/shard builds a consistent-hash ring from
// RISK_ENGINE_SHARD_MEMBERS and places this replica on it with
// RISK_ENGINE_SHARD_SELF. Ownership is a pure function of (members, self), and
// it is only a PARTITION — every portfolio owned by exactly one replica — when
// the list names exactly the pods that are running. Neither half of that is
// self-correcting:
//
//   - A pod NOT in the list owns nothing. Its ShardFilter drops every event, so
//     it consumes and acks the whole spine and applies none of it. Since #110's
//     repair it refuses to start instead, which is a crashloop rather than a
//     silent one — still an outage of that replica.
//   - A member in the list with no pod behind it owns portfolios that NOBODY
//     processes. Nothing errors, no probe fails, no DLQ fills: those portfolios
//     simply stop being revalued, and their last risk measure ages in place
//     looking exactly like a current one.
//
// KEDA changes the replica count without changing the env, so the second case
// is guaranteed on the first scale-in and the first case on the first scale-out.
// That is why the risk-engine Rollout deliberately sets NEITHER variable today
// and runs unsharded (see internal/app/shard_posture.go, which WARNs that the
// fleet is therefore computing from partial books). Wiring a hand-written list
// into a KEDA-scaled workload looks like the fix for #110 and is strictly worse
// than the state it replaces.
//
// So the rule is the pairing, not the presence: a workload may carry a shard
// member list only if its replica count is FIXED and the list matches it. Until
// membership discovery exists, that means no autoscaled workload carries one.
func TestShardMemberListIsNeverStaticOnAnAutoscaledWorkload(t *testing.T) {
	root := moduleRoot(t)
	workloads, scaled := shardManifests(t, root)

	// NON-VACUITY, and it is not optional here: every assertion below is
	// conditional on finding an env var that NOTHING in the estate sets today.
	// A parser that silently read no workloads, no env blocks, or no Rollouts
	// would pass this test forever while checking nothing — which is the exact
	// shape of false green this repository keeps hitting.
	if len(workloads) < 20 {
		t.Fatalf("scanned infra/deploy and found only %d workloads — this estate has far more. "+
			"Finding almost none is a BROKEN SCAN reported as a clean result; did infra/deploy "+
			"move, or did the parse stop early?", len(workloads))
	}
	if len(scaled) < 5 {
		t.Fatalf("found only %d KEDA ScaledObjects across infra/ — there were six when this guard "+
			"was written (alternatives, copilot, datamaster, market-data, risk-engine, wealth). "+
			"Finding almost none means the autoscaler side of this rule is not being read.", len(scaled))
	}

	// PROVE THE PARSER REACHES THE ONE DOCUMENT THIS GUARD EXISTS FOR. risk-engine
	// is an Argo Rollout, not a Deployment, and its env is the thing under rule.
	// A parser that handled only Deployments — or read `kind` but never
	// `spec.template.spec.containers[].env` — would leave this guard green with
	// the whole subject invisible to it.
	re, ok := workloads[workloadRef{Kind: "Rollout", Name: "risk-engine"}]
	if !ok {
		t.Fatal("infra/deploy has no Rollout named risk-engine — this guard's subject is not being " +
			"parsed at all. Every assertion below is conditional, so it would pass vacuously.")
	}
	if _, ok := re.env["RISK_ENGINE_NATS_URL"]; !ok {
		t.Fatalf("%s: parsed the risk-engine Rollout but read no RISK_ENGINE_NATS_URL from its "+
			"containers — the env block is not being reached, so a shard member list set there "+
			"would be invisible to this guard. Parsed env keys: %v", re.file, sortedKeys(re.env))
	}
	if _, ok := scaled[workloadRef{Kind: "Rollout", Name: "risk-engine"}]; !ok {
		t.Fatal("no ScaledObject targets the risk-engine Rollout — scaleTargetRef is not being " +
			"resolved to the workload it scales, which is half of this rule.")
	}

	var problems []string
	for ref, w := range workloads {
		members, hasMembers := w.shardEnv("_SHARD_MEMBERS")
		selfKey, hasSelf := w.shardEnv("_SHARD_SELF")

		if !hasMembers && !hasSelf {
			continue
		}

		// Both halves or neither. Since #110 the service refuses to start on
		// either alone, so a one-sided edit is a crashloop, not a degradation.
		switch {
		case hasMembers && !hasSelf:
			problems = append(problems, fmt.Sprintf(
				"%s: %s %q sets %s but no *_SHARD_SELF — the replica has a ring and no id on it, "+
					"which shard.NewAssignment refuses (ErrSelfUnset) and the process exits",
				w.file, ref.Kind, ref.Name, members.key))
		case hasSelf && !hasMembers:
			problems = append(problems, fmt.Sprintf(
				"%s: %s %q sets %s but no *_SHARD_MEMBERS — the replica has an id and no ring, "+
					"which shard.NewAssignment refuses (ErrMembersUnset) and the process exits",
				w.file, ref.Kind, ref.Name, selfKey.key))
		}
		if !hasMembers {
			continue
		}

		if so, autoscaled := scaled[ref]; autoscaled {
			problems = append(problems, fmt.Sprintf(
				"%s: %s %q sets a static %s (%d members) while ScaledObject %q autoscales it "+
					"%s. The ring is a partition only while the list names exactly the running "+
					"pods: on scale-out the new pods are not in the list and refuse to start, and "+
					"on scale-in the departed members' portfolios stop being revalued with nothing "+
					"erroring. Wire membership DISCOVERY or pin the replica count — do not hand-write "+
					"a list for an autoscaled workload (#110)",
				w.file, ref.Kind, ref.Name, members.key, len(members.list), so.name, so.bounds()))
			continue
		}

		if w.replicas == nil {
			problems = append(problems, fmt.Sprintf(
				"%s: %s %q sets %s (%d members) but declares no spec.replicas, so the list cannot "+
					"be checked against the fleet it claims to describe (#110)",
				w.file, ref.Kind, ref.Name, members.key, len(members.list)))
			continue
		}
		if len(members.list) != *w.replicas {
			problems = append(problems, fmt.Sprintf(
				"%s: %s %q sets %s with %d members (%v) but spec.replicas is %d. A member with no "+
					"pod behind it owns portfolios nobody processes — they stop being revalued and "+
					"their last measure ages in place looking current (#110)",
				w.file, ref.Kind, ref.Name, members.key, len(members.list), members.list, *w.replicas))
		}
	}

	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// workloadRef identifies a workload the way a ScaledObject's scaleTargetRef
// does — by kind and name — so the two sides join without a name-only match
// that would confuse a Deployment with the Rollout that replaced it.
type workloadRef struct {
	Kind string
	Name string
}

type shardWorkload struct {
	file     string
	replicas *int
	// env is every literal container env var on the workload (initContainers
	// included). A valueFrom entry is recorded with an empty value: it is SET,
	// which is what the pairing rule turns on, but its content is not knowable
	// from the manifest.
	env map[string]string
}

type shardEnvVar struct {
	key  string
	list []string
}

// shardEnv finds the single env var whose name ends in suffix. The prefix is
// the service's own (RISK_ENGINE_), so this matches whatever a future service
// names its ring without a second copy of the list of services here.
func (w shardWorkload) shardEnv(suffix string) (shardEnvVar, bool) {
	for _, k := range sortedKeys(w.env) {
		if !strings.HasSuffix(k, suffix) {
			continue
		}
		var list []string
		for _, p := range strings.Split(w.env[k], ",") {
			if p = strings.TrimSpace(p); p != "" {
				list = append(list, p)
			}
		}
		return shardEnvVar{key: k, list: list}, true
	}
	return shardEnvVar{}, false
}

type shardScaledObject struct {
	name string
	min  *int
	max  *int
}

func (s shardScaledObject) bounds() string {
	lo, hi := "?", "?"
	if s.min != nil {
		lo = fmt.Sprint(*s.min)
	}
	if s.max != nil {
		hi = fmt.Sprint(*s.max)
	}
	return fmt.Sprintf("from %s to %s replicas", lo, hi)
}

type shardManifestDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Replicas *int `yaml:"replicas"`
		Template struct {
			Spec struct {
				Containers     []shardContainer `yaml:"containers"`
				InitContainers []shardContainer `yaml:"initContainers"`
			} `yaml:"spec"`
		} `yaml:"template"`
		// ScaledObject
		MinReplicaCount *int `yaml:"minReplicaCount"`
		MaxReplicaCount *int `yaml:"maxReplicaCount"`
		ScaleTargetRef  struct {
			Kind string `yaml:"kind"`
			Name string `yaml:"name"`
		} `yaml:"scaleTargetRef"`
	} `yaml:"spec"`
}

type shardContainer struct {
	Env []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"env"`
}

// shardWorkloadKinds are the kinds a KEDA ScaledObject can drive and that carry
// a pod template — the only documents where a shard member list can be set.
var shardWorkloadKinds = map[string]bool{
	"Deployment":  true,
	"StatefulSet": true,
	"Rollout":     true,
	"DaemonSet":   true,
}

// shardManifests parses every YAML document under infra/ and returns the
// workloads by (kind, name) plus the ScaledObjects keyed by the workload they
// target. Parsed structurally with yaml.v3 rather than grepped: `SHARD` appears
// in a prose comment in market-ingest-deploy.yaml, and a regex cannot tell a
// container's env from a paragraph about sharding.
func shardManifests(t *testing.T, root string) (map[workloadRef]shardWorkload, map[workloadRef]shardScaledObject) {
	t.Helper()
	base := filepath.Join(root, "infra")

	workloads := map[workloadRef]shardWorkload{}
	scaled := map[workloadRef]shardScaledObject{}

	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !(strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")) {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)

		dec := yaml.NewDecoder(strings.NewReader(readFile(t, p)))
		for {
			var doc shardManifestDoc
			derr := dec.Decode(&doc)
			if errors.Is(derr, io.EOF) {
				break
			}
			if derr != nil {
				// infra/ carries Helm-templated and non-Kubernetes YAML too;
				// a document this decoder cannot read carries no workload for
				// this rule. Stop reading THIS file rather than failing the
				// estate, and keep walking.
				break
			}
			ref := workloadRef{Kind: doc.Kind, Name: doc.Metadata.Name}
			if ref.Name == "" {
				continue
			}
			switch {
			case shardWorkloadKinds[doc.Kind]:
				env := map[string]string{}
				cs := append(append([]shardContainer{}, doc.Spec.Template.Spec.Containers...),
					doc.Spec.Template.Spec.InitContainers...)
				for _, c := range cs {
					for _, e := range c.Env {
						if e.Name != "" {
							env[e.Name] = e.Value
						}
					}
				}
				workloads[ref] = shardWorkload{file: rel, replicas: doc.Spec.Replicas, env: env}
			case doc.Kind == "ScaledObject":
				target := workloadRef{Kind: doc.Spec.ScaleTargetRef.Kind, Name: doc.Spec.ScaleTargetRef.Name}
				if target.Name == "" {
					continue
				}
				scaled[target] = shardScaledObject{
					name: ref.Name,
					min:  doc.Spec.MinReplicaCount,
					max:  doc.Spec.MaxReplicaCount,
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk infra/: %v", err)
	}
	return workloads, scaled
}

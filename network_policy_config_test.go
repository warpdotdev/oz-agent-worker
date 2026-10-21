package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/warpdotdev/oz-agent-worker/internal/config"
	"gopkg.in/yaml.v3"
)

func TestNetworkPolicyConfigAndBackendAdmission(t *testing.T) {
	for _, block := range []string{"", "    network_policy:\n      dns:\n        namespace: kube-system\n", "    network_policy:\n      enabled: true\n      dns:\n        server_ips: [192.0.2.53, '2001:db8::53']\n        namespace: trusted-dns\n        pod_labels: {app: resolver}\n"} {
		t.Run(strings.ReplaceAll(block, "\n", " "), func(t *testing.T) {
			resetCLIForTest()
			t.Cleanup(resetCLIForTest)
			path := filepath.Join(t.TempDir(), "worker.yaml")
			raw := "worker_id: worker\nbackend:\n  kubernetes:\n    namespace: agents\n" + block
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			loaded, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			merged, err := mergeConfig(loaded)
			if err != nil {
				t.Fatal(err)
			}
			policy := merged.Kubernetes.NetworkPolicy
			if policy.Enabled != strings.Contains(block, "enabled: true") || !reflect.DeepEqual(policy, loaded.Backend.Kubernetes.NetworkPolicy) {
				t.Fatalf("network policy configuration lost: %+v", policy)
			}
			if policy.Enabled && (!reflect.DeepEqual(policy.DNS.ServerIPs, []string{"192.0.2.53", "2001:db8::53"}) || policy.DNS.Namespace != "trusted-dns" || policy.DNS.PodLabels["app"] != "resolver") {
				t.Fatalf("DNS configuration lost: %+v", policy)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "reserved.yaml")
	if err := os.WriteFile(path, []byte("backend:\n  kubernetes:\n    extra_labels: {warp-custom: forbidden}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved label accepted: %v", err)
	}
}

func TestNetworkPolicyHelm(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required for chart acceptance tests")
	}
	for _, tc := range []struct {
		name    string
		args    []string
		enabled bool
		noRole  bool
	}{
		{name: "default"},
		{name: "disabled", args: []string{"--set", "kubernetesBackend.networkPolicy.enabled=false"}},
		{name: "enabled", args: []string{"--set", "kubernetesBackend.networkPolicy.enabled=true"}, enabled: true},
		{name: "external RBAC", args: []string{"--set", "kubernetesBackend.networkPolicy.enabled=true", "--set", "rbac.create=false"}, enabled: true, noRole: true},
		{name: "DNS override", args: []string{"--set", "kubernetesBackend.networkPolicy.enabled=true", "--set", "kubernetesBackend.networkPolicy.dns.serverIPs[0]=192.0.2.53", "--set", "kubernetesBackend.networkPolicy.dns.namespace=trusted-dns", "--set", "kubernetesBackend.networkPolicy.dns.podLabels.app=resolver"}, enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"template", "oz-agent-worker", "./charts/oz-agent-worker", "--namespace", "agents", "--set", "worker.workerId=ci-worker", "--set", "image.tag=ci"}, tc.args...)
			raw, err := exec.Command(helm, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("helm: %v %s", err, raw)
			}
			decoder := yaml.NewDecoder(bytes.NewReader(raw))
			baselines, roles, policyRules := 0, 0, 0
			for {
				var object map[string]any
				err := decoder.Decode(&object)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				switch object["kind"] {
				case "NetworkPolicy":
					baselines++
					spec := object["spec"].(map[string]any)
					want := map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{"warp-network-restricted": "true"}}, "policyTypes": []any{"Egress"}, "egress": []any{}}
					if !reflect.DeepEqual(spec, want) {
						t.Fatalf("baseline=%v", spec)
					}
				case "Role":
					roles++
					for _, rawRule := range object["rules"].([]any) {
						rule := rawRule.(map[string]any)
						resource := rule["resources"].([]any)[0].(string)
						if resource == "networkpolicies" || resource == "ciliumnetworkpolicies" {
							policyRules++
							if !reflect.DeepEqual(rule["verbs"], []any{"create", "get", "list", "patch", "delete"}) {
								t.Fatalf("policy RBAC=%v", rule)
							}
						}
						if resource == "services" {
							t.Fatal("DNS service permissions added")
						}
					}
				case "ClusterRole":
					t.Fatal("cluster-scoped policy rights added")
				case "ConfigMap":
					data := object["data"].(map[string]any)["config.yaml"].(string)
					path := filepath.Join(t.TempDir(), "rendered.yaml")
					if err := os.WriteFile(path, []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
					cfg, err := config.Load(path)
					if err != nil {
						t.Fatal(err)
					}
					policy := cfg.Backend.Kubernetes.NetworkPolicy
					if policy.Enabled != tc.enabled {
						t.Fatalf("enabled=%v", policy.Enabled)
					}
					if tc.name == "DNS override" && (policy.DNS.Namespace != "trusted-dns" || policy.DNS.PodLabels["app"] != "resolver" || !reflect.DeepEqual(policy.DNS.ServerIPs, []string{"192.0.2.53"})) {
						t.Fatalf("DNS=%+v", policy.DNS)
					}
				}
			}
			wantRules := 0
			if tc.enabled && !tc.noRole {
				wantRules = 2
			}
			if baselines != 1 || policyRules != wantRules || (tc.noRole && roles != 0) {
				t.Fatalf("baseline=%d role=%d policy rules=%d", baselines, roles, policyRules)
			}
		})
	}
	cmd := exec.Command(helm, "template", "worker", "./charts/oz-agent-worker", "--set", "worker.workerId=ci-worker", "--set", "image.tag=ci", "--set", "kubernetesBackend.extraLabels.warp-custom=forbidden")
	raw, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(raw), "reserved for Warp") {
		t.Fatalf("Helm accepted reserved label: %s %v", raw, err)
	}
}

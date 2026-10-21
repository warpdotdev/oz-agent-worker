package worker

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/config"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

const (
	networkRestrictedLabel = "warp-network-restricted"
	networkPolicyIDLabel = "warp-network-policy-id"
	networkManagedLabel = "warp-network-managed"
	networkCanonicalAnnotation = "warp-network-policy-canonical"
	networkConsumersAnnotation = "warp-network-policy-consumers"
	networkPolicyUIDAnnotation = "warp-network-policy-uid"
	networkRunAnnotation = "warp-run-id"
	networkExecutionAnnotation = "warp-execution-id"
	networkPolicyFinalizer = "warp.dev/network-policy-consumers"
	networkPolicyPlaceholder = "POLICY_ID"
)

var ciliumNetworkPolicyResource = schema.GroupVersionResource{
	Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies",
}

func (w *Worker) admitNetworkPolicy(policy *types.NetworkPolicy) (string, error) {
	if err := validateNetworkPolicy(policy); err != nil {
		return "network_policy_invalid", err
	}
	backend, ok := w.backend.(*KubernetesBackend)
	if !ok || !backend.config.NetworkPolicy.Enabled {
		return "network_policy_unsupported", fmt.Errorf("restricted executions require the Kubernetes backend with backend.kubernetes.network_policy.enabled: true and an operator-verified enforcing baseline")
	}
	return "", nil
}

func validateNetworkPolicy(policy *types.NetworkPolicy) error {
	if policy == nil {
		return nil
	}
	for i, rule := range policy.Rules {
		if rule.Action != "ALLOW" || len(rule.Hosts) == 0 {
			return fmt.Errorf("network policy rule %d requires action ALLOW and nonempty hosts", i)
		}
		for _, host := range rule.Hosts {
			if ip, err := netip.ParseAddr(host); err == nil {
				if ip.Zone() == "" && ip.String() == host {
					continue
				}
				return fmt.Errorf("network policy IP %q must be canonical and have no zone", host)
			}
			if host == "" || len(host) > 253 || host != strings.ToLower(host) {
				return fmt.Errorf("network policy host %q must be a canonical literal hostname or IP", host)
			}
			for _, label := range strings.Split(host, ".") {
				if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
					return fmt.Errorf("invalid network policy hostname %q", host)
				}
				for _, char := range label {
					if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
						return fmt.Errorf("invalid network policy hostname %q", host)
					}
				}
			}
		}
	}
	return nil
}

func effectiveNetworkPolicyDNS(cfg config.NetworkPolicyDNSConfig, inCluster bool, resolvConf []byte) (config.NetworkPolicyDNSConfig, error) {
	if cfg.Namespace == "" {
		cfg.Namespace = "kube-system"
	}
	if cfg.PodLabels == nil {
		cfg.PodLabels = map[string]string{"k8s-app": "kube-dns"}
	}
	if len(validation.IsDNS1123Label(cfg.Namespace)) != 0 || len(cfg.PodLabels) == 0 {
		return cfg, fmt.Errorf("network_policy.dns requires a valid namespace and nonempty pod_labels")
	}
	for key, value := range cfg.PodLabels {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return cfg, fmt.Errorf("invalid network_policy.dns.pod_labels entry %q", key)
		}
	}
	ips := cfg.ServerIPs
	if len(ips) == 0 {
		if !inCluster {
			return cfg, fmt.Errorf("out-of-cluster workers must set backend.kubernetes.network_policy.dns.server_ips")
		}
		for line := range strings.SplitSeq(string(resolvConf), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "nameserver" {
				ips = append(ips, fields[1])
			}
		}
	}
	var usable []string
	for _, value := range ips {
		ip, err := netip.ParseAddr(value)
		if err != nil || ip.Zone() != "" || ip.IsLoopback() || ip.IsUnspecified() {
			return cfg, fmt.Errorf("unusable DNS resolver %q; set backend.kubernetes.network_policy.dns.server_ips to trusted task resolvers", value)
		}
		usable = append(usable, ip.Unmap().String())
	}
	slices.Sort(usable)
	cfg.ServerIPs = slices.Compact(usable)
	if len(cfg.ServerIPs) == 0 {
		return cfg, fmt.Errorf("no usable in-cluster DNS nameservers; set backend.kubernetes.network_policy.dns.server_ips")
	}
	return cfg, nil
}

func (b *KubernetesBackend) networkPolicyDNS() (config.NetworkPolicyDNSConfig, error) {
	cfg := b.config.NetworkPolicy.DNS
	var data []byte
	if len(cfg.ServerIPs) == 0 && b.networkInCluster {
		var err error
		data, err = os.ReadFile("/etc/resolv.conf")
		if err != nil {
			return cfg, fmt.Errorf("read cluster DNS: %w; set backend.kubernetes.network_policy.dns.server_ips", err)
		}
	}
	return effectiveNetworkPolicyDNS(cfg, b.networkInCluster, data)
}

type networkHostResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

func networkDNSResolver(ips []string) *net.Resolver {
	var next atomic.Uint64
	return &net.Resolver{PreferGo: true, StrictErrors: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		ip := ips[(next.Add(1)-1)%uint64(len(ips))]
		dialer := net.Dialer{Timeout: 5*time.Second}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ip, "53"))
	}}
}

type networkPolicyProvider struct {
	backend *KubernetesBackend
	cilium bool
}

func (b *KubernetesBackend) discoverNetworkPolicyProvider() (networkPolicyProvider, error) {
	provider := networkPolicyProvider{backend: b}
	if b.networkDiscovery == nil {
		return provider, fmt.Errorf("network policy discovery client is unavailable")
	}
	resources, err := b.networkDiscovery.ServerResourcesForGroupVersion("cilium.io/v2")
	if apierrors.IsNotFound(err) {
		return provider, nil
	}
	if err != nil {
		return provider, fmt.Errorf("discover Cilium policies (native fallback requires definite API absence): %w", err)
	}
	if resources == nil {
		return provider, fmt.Errorf("Cilium discovery returned no resource list")
	}
	for _, resource := range resources.APIResources {
		if resource.Name == ciliumNetworkPolicyResource.Resource {
			provider.cilium = true
			if b.networkDynamic == nil {
				return provider, fmt.Errorf("Cilium dynamic client is unavailable")
			}
			break
		}
	}
	return provider, nil
}

func compileNetworkPolicy(ctx context.Context, policy *types.NetworkPolicy, cilium bool, dns config.NetworkPolicyDNSConfig, resolver networkHostResolver) (*unstructured.Unstructured, error) {
	if policy == nil {
		return nil, fmt.Errorf("restricted launch requires a present policy")
	}
	if err := validateNetworkPolicy(policy); err != nil {
		return nil, err
	}
	var names, cidrs []string
	for _, rule := range policy.Rules {
		for _, host := range rule.Hosts {
			if ip, err := netip.ParseAddr(host); err == nil {
				cidrs = append(cidrs, exactNetworkCIDR(ip))
			} else {
				names = append(names, host)
			}
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) > 0 {
		var err error
		dns, err = effectiveNetworkPolicyDNS(dns, false, nil)
		if err != nil {
			return nil, err
		}
	}
	if !cilium {
		for _, name := range names {
			if resolver == nil {
				return nil, fmt.Errorf("no resolver for network policy host %s", name)
			}
			lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			ips, err := resolver.LookupNetIP(lookupCtx, "ip", name)
			cancel()
			if err != nil || len(ips) == 0 {
				return nil, fmt.Errorf("resolve network policy host %s: no complete DNS answer (%v)", name, err)
			}
			for _, ip := range ips {
				if !ip.IsValid() || ip.Zone() != "" {
					return nil, fmt.Errorf("invalid DNS answer for %s", name)
				}
				cidrs = append(cidrs, exactNetworkCIDR(ip))
			}
		}
	}
	slices.Sort(cidrs)
	cidrs = slices.Compact(cidrs)
	selector := map[string]any{"matchLabels": map[string]any{
		networkRestrictedLabel: "true", networkPolicyIDLabel: networkPolicyPlaceholder,
	}}
	spec := map[string]any{}
	var egress []any
	kind, apiVersion := "NetworkPolicy", "networking.k8s.io/v1"
	if cilium {
		kind, apiVersion = "CiliumNetworkPolicy", "cilium.io/v2"
		spec["endpointSelector"] = selector
		spec["enableDefaultDeny"] = map[string]any{"egress": true}
		if len(cidrs) > 0 {
			egress = append(egress, map[string]any{"toCIDR": stringAnySlice(cidrs)})
		}
		if len(names) > 0 {
			var fqdns, queries []any
			for _, name := range names {
				fqdns = append(fqdns, map[string]any{"matchName": name})
				queries = append(queries, map[string]any{"matchName": name})
			}
			egress = append(egress, map[string]any{"toFQDNs": fqdns})
			dnsLabels := map[string]any{"k8s:io.kubernetes.pod.namespace": dns.Namespace}
			for key, value := range dns.PodLabels {
				dnsLabels["k8s:"+key] = value
			}
			egress = append(egress, map[string]any{
				"toEndpoints": []any{map[string]any{"matchLabels": dnsLabels}},
				"toPorts": []any{map[string]any{
					"ports": []any{map[string]any{"port": "53", "protocol": "UDP"}, map[string]any{"port": "53", "protocol": "TCP"}},
					"rules": map[string]any{"dns": queries},
				}},
			})
		}
	} else {
		spec["podSelector"] = selector
		spec["policyTypes"] = []any{"Egress"}
		if len(cidrs) > 0 {
			var peers []any
			for _, cidr := range cidrs {
				peers = append(peers, map[string]any{"ipBlock": map[string]any{"cidr": cidr}})
			}
			egress = append(egress, map[string]any{"to": peers})
		}
		if len(names) > 0 {
			var peers []any
			for _, ip := range dns.ServerIPs {
				addr, _ := netip.ParseAddr(ip)
				peers = append(peers, map[string]any{"ipBlock": map[string]any{"cidr": exactNetworkCIDR(addr)}})
			}
			podLabels := make(map[string]any, len(dns.PodLabels))
			for key, value := range dns.PodLabels {
				podLabels[key] = value
			}
			peers = append(peers, map[string]any{
				"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": dns.Namespace}},
				"podSelector": map[string]any{"matchLabels": podLabels},
			})
			egress = append(egress, map[string]any{"to": peers, "ports": []any{
				map[string]any{"port": int64(53), "protocol": "UDP"},
				map[string]any{"port": int64(53), "protocol": "TCP"},
			}})
		}
	}
	if len(egress) > 0 {
		spec["egress"] = egress
	}
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "spec": spec}}
	canonical, err := canonicalNetworkSpec(object, networkPolicyPlaceholder)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	id := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))
	selector["matchLabels"].(map[string]any)[networkPolicyIDLabel] = id
	object.SetName("warp-np-"+id)
	object.SetLabels(map[string]string{networkManagedLabel: "true", networkPolicyIDLabel: id})
	object.SetAnnotations(map[string]string{networkCanonicalAnnotation: string(canonical)})
	object.SetFinalizers([]string{networkPolicyFinalizer})
	return object, nil
}

func exactNetworkCIDR(ip netip.Addr) string {
	ip = ip.Unmap()
	return netip.PrefixFrom(ip, ip.BitLen()).String()
}

func stringAnySlice(values []string) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

func canonicalNetworkSpec(object *unstructured.Unstructured, expectedID string) ([]byte, error) {
	spec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("policy spec is absent or malformed")
	}
	selectorKey := "podSelector"
	if object.GetKind() == "CiliumNetworkPolicy" {
		selectorKey = "endpointSelector"
	}
	id, _, err := unstructured.NestedString(spec, selectorKey, "matchLabels", networkPolicyIDLabel)
	if err != nil || id != expectedID {
		return nil, fmt.Errorf("policy generated selector does not match %q", expectedID)
	}
	if err := unstructured.SetNestedField(spec, networkPolicyPlaceholder, selectorKey, "matchLabels", networkPolicyIDLabel); err != nil {
		return nil, err
	}
	if object.GetKind() == "NetworkPolicy" {
		var typed networkingv1.NetworkPolicySpec
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &typed); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(typed)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(encoded, &spec); err != nil {
			return nil, err
		}
	}
	return json.Marshal(canonicalNetworkSets(spec))
}

func canonicalNetworkSets(value any) any {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			value[key] = canonicalNetworkSets(item)
		}
		return value
	case []any:
		items := make(map[string]any, len(value))
		var keys []string
		for _, item := range value {
			item = canonicalNetworkSets(item)
			key, _ := json.Marshal(item)
			if _, exists := items[string(key)]; !exists {
				keys = append(keys, string(key))
				items[string(key)] = item
			}
		}
		slices.Sort(keys)
		result := make([]any, 0, len(keys))
		for _, key := range keys {
			result = append(result, items[key])
		}
		return result
	default:
		return value
	}
}

func (p networkPolicyProvider) ciliumClient() dynamic.ResourceInterface {
	return p.backend.networkDynamic.Resource(ciliumNetworkPolicyResource).Namespace(p.backend.config.Namespace)
}

func nativePolicyObject(policy *networkingv1.NetworkPolicy) (*unstructured.Unstructured, error) {
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(policy)
	if err != nil {
		return nil, err
	}
	result := &unstructured.Unstructured{Object: object}
	result.SetKind("NetworkPolicy")
	return result, nil
}

func (p networkPolicyProvider) get(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	if p.cilium {
		return p.ciliumClient().Get(ctx, name, metav1.GetOptions{})
	}
	object, err := p.backend.clientset.NetworkingV1().NetworkPolicies(p.backend.config.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return nativePolicyObject(object)
}

func (p networkPolicyProvider) create(ctx context.Context, object *unstructured.Unstructured) error {
	if p.cilium {
		_, err := p.ciliumClient().Create(ctx, object, metav1.CreateOptions{})
		return err
	}
	var typed networkingv1.NetworkPolicy
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &typed); err != nil {
		return err
	}
	_, err := p.backend.clientset.NetworkingV1().NetworkPolicies(p.backend.config.Namespace).Create(ctx, &typed, metav1.CreateOptions{})
	return err
}

func (p networkPolicyProvider) patchMetadata(ctx context.Context, object *unstructured.Unstructured) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"uid": object.GetUID(), "resourceVersion": object.GetResourceVersion(),
		"annotations": object.GetAnnotations(), "finalizers": object.GetFinalizers(),
	}})
	if err != nil {
		return err
	}
	if p.cilium {
		_, err = p.ciliumClient().Patch(ctx, object.GetName(), k8stypes.MergePatchType, patch, metav1.PatchOptions{})
	} else {
		_, err = p.backend.clientset.NetworkingV1().NetworkPolicies(p.backend.config.Namespace).Patch(ctx, object.GetName(), k8stypes.MergePatchType, patch, metav1.PatchOptions{})
	}
	return err
}

func (p networkPolicyProvider) delete(ctx context.Context, object *unstructured.Unstructured) error {
	uid, rv := object.GetUID(), object.GetResourceVersion()
	options := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}
	if p.cilium {
		return p.ciliumClient().Delete(ctx, object.GetName(), options)
	}
	return p.backend.clientset.NetworkingV1().NetworkPolicies(p.backend.config.Namespace).Delete(ctx, object.GetName(), options)
}

func (p networkPolicyProvider) list(ctx context.Context) ([]unstructured.Unstructured, error) {
	options := metav1.ListOptions{LabelSelector: networkManagedLabel+"=true"}
	if p.cilium {
		list, err := p.ciliumClient().List(ctx, options)
		if err != nil {
			return nil, err
		}
		return list.Items, nil
	}
	list, err := p.backend.clientset.NetworkingV1().NetworkPolicies(p.backend.config.Namespace).List(ctx, options)
	if err != nil {
		return nil, err
	}
	var result []unstructured.Unstructured
	for i := range list.Items {
		object, err := nativePolicyObject(&list.Items[i])
		if err != nil {
			return nil, err
		}
		result = append(result, *object)
	}
	return result, nil
}

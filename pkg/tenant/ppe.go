/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tenant

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// PPE policy-engine POC (see docs/proposals/praxis-policy-engine-adoption.md).
//
// When a tenant's MaasTenantConfig carries
// AnnotationPolicyEngine=PolicyEnginePPE, the pre-auth ExtProc chain gains a
// `policy` filter (the embedded Praxis Policy Engine, praxis-extproc
// `policy-engine` cargo feature) that runs after the BBR chain, in the
// position the RHCL Kuadrant wasm filter occupies today. The policy document
// it loads is rendered into the same plugins ConfigMap under
// PPDPolicyConfigMapKey, so the existing sha256-roll mechanism reloads the
// Deployment on change.
//
// POC carve-outs (deliberate, documented in the ADR):
//   - identity is API-key only, resolved against maas-api's
//     /internal/v1/api-keys/validate contract over the host transport;
//   - rate limiting is the in-process ratelimit/limitador plugin keyed on the
//     subscription, so counters are per-replica (single-replica gateways only);
//   - /maas-api paths are allowed through unauthenticated: maas-api keeps
//     validating OpenShift tokens itself (API-key minting is unchanged);
//   - no TokenReview, no OIDC, no per-model TRLP.
//
// Image requirements (the stock odh-praxis-extproc FIPS image will NOT work):
// the policy filter needs the `policy-engine` cargo feature (excluded from
// FIPS builds), praxis 0.8.0 / praxis-ai v0.11.0 or newer (PPE 0.6.0:
// `rate_limits` + maas-api http directory), `policy-experimental-ratelimit`
// for ratelimit/limitador, and `panic = "unwind"` in the release profile
// (PPE refuses to load Limitador under panic=abort). See the
// early-praxis-policy-engine-ppe branch of praxis-extproc.

// PPDPolicyConfigMapKey is the plugins ConfigMap data key holding the PPE
// policy document.
const PPDPolicyConfigMapKey = "ppe-policy.yaml"

// PPDPolicyConfigPath is where the ConfigMap mount projects that key inside
// the pre-auth ExtProc container (plugins-config-volume mounts /etc/praxis).
const PPDPolicyConfigPath = "/etc/praxis/ppe-policy.yaml"

// preAuthPolicyFilterYAML is appended to the vendored pre-auth chain
// (pre-extproc.yaml, chain "bbr") when PPE is enabled. It runs last in the
// chain: BBR promotes the model header first, then policy enforces
// authn/authz/rate limits before Envoy routes.
const preAuthPolicyFilterYAML = `      - filter: policy
        config_path: ` + PPDPolicyConfigPath + `
        # maas-api's in-cluster address is private; the policy filter is
        # public-endpoint-only by default, so relax exactly this host.
        trusted_private_endpoints:
          - %s
`

// TenantUsesPPE reports whether the MaasTenantConfig opts this tenant into
// the Praxis Policy Engine dataplane (AnnotationPolicyEngine=ppe). Absent,
// empty, or any other value keeps the default RHCL path.
func TenantUsesPPE(mtc *unstructured.Unstructured) bool {
	if mtc == nil {
		return false
	}
	value, ok := mtc.GetAnnotations()[AnnotationPolicyEngine]
	return ok && strings.TrimSpace(value) == PolicyEnginePPE
}

// configureExtProcPolicyEngine injects the `policy` filter into the pre-auth
// chain and renders the PPE policy document into the plugins ConfigMap.
// maasAPIURL is maas-api's in-cluster validate endpoint
// (https://maas-api.<ns>.svc:8443/internal/v1/api-keys/validate);
// maasAPIHost is its URL host, allow-listed for the policy filter's private
// endpoint restriction.
func configureExtProcPolicyEngine(resources []unstructured.Unstructured, maasAPIURL, maasAPIHost, tenantID string, maxBodyBytes int64) error {
	patched := 0
	for i := range resources {
		cm := &resources[i]
		if cm.GetKind() != "ConfigMap" || !isOwnedPluginConfigMap(cm.GetName()) {
			continue
		}
		data, found, err := unstructured.NestedStringMap(cm.Object, "data")
		if err != nil {
			return fmt.Errorf("ConfigMap %s/%s: read data: %w", cm.GetNamespace(), cm.GetName(), err)
		}
		if !found {
			continue
		}
		pre, ok := data["pre-extproc.yaml"]
		if !ok {
			continue
		}
		// Inject the policy filter first (indented under the chain), then the
		// praxis 0.8.0 patches: patchPreChainForPraxis08 appends a top-level
		// insecure_options key, which must come after the chain block.
		if !strings.Contains(pre, "filter: policy") {
			pre += fmt.Sprintf(preAuthPolicyFilterYAML, maasAPIHost)
		}
		pre, _ = patchPreChainForPraxis08(pre, maxBodyBytes)
		data["pre-extproc.yaml"] = pre
		data[PPDPolicyConfigMapKey] = ppePolicyDocument(maasAPIURL, tenantID)
		if err := unstructured.SetNestedStringMap(cm.Object, data, "data"); err != nil {
			return fmt.Errorf("ConfigMap %s/%s: write data: %w", cm.GetNamespace(), cm.GetName(), err)
		}
		patched++
	}
	if patched == 0 {
		return fmt.Errorf("no plugin ConfigMap pre-extproc.yaml found to inject the policy filter for tenant %s", tenantID)
	}
	// The rendered ext_proc NetworkPolicy only allows DNS, the gateway and
	// the Kubernetes API; PPE's api-key directory needs maas-api:8443, and
	// the ext_proc pods need the gateway label maas-api's ingress policy
	// matches on.
	configurePPENetworkPolicy(resources, gatewayNamespaceOf(resources))
	if err := stampPreDeploymentHash(resources, tenantID, maasAPIURL); err != nil {
		return err
	}
	return nil
}

// collapseExtProcForPPE realizes the single-proxy-instance shape (ADR
// section 2) for a PPE tenant. Called from the reconciler after
// configureExtProcPolicyEngine.
func collapseExtProcForPPE(resources []unstructured.Unstructured, tenantID string) ([]unstructured.Unstructured, error) {
	return collapsePreIntoPost(resources, tenantID)
}

// collapsePreIntoPost merges the pre-auth chain (BBR + policy filter) into
// the post-auth chain and drops the pre-auth Deployment, realizing the ADR's
// single-proxy-instance target shape for PPE tenants: one ExtProc
// Deployment, one config, one Envoy ext_proc filter. The praxis server
// concatenates every filter_chains entry in order, so prepending the pre
// filters to the post chain preserves the wasm-era ordering (BBR → policy →
// payload processing).
func collapsePreIntoPost(resources []unstructured.Unstructured, tenantID string) ([]unstructured.Unstructured, error) {
	// 1. Merge the chains in the plugins ConfigMap.
	var preFilters []interface{}
	for i := range resources {
		cm := &resources[i]
		if cm.GetKind() != "ConfigMap" || !isOwnedPluginConfigMap(cm.GetName()) {
			continue
		}
		data, found, err := unstructured.NestedStringMap(cm.Object, "data")
		if err != nil || !found {
			continue
		}
		pre, ok := data["pre-extproc.yaml"]
		if !ok {
			continue
		}
		post, ok := data["extproc.yaml"]
		if !ok {
			continue
		}
		merged, filters, err := mergePraxisChains(pre, post)
		if err != nil {
			return nil, fmt.Errorf("merge pre/post chains: %w", err)
		}
		preFilters = filters
		data["extproc.yaml"] = merged
		delete(data, "pre-extproc.yaml")
		if err := unstructured.SetNestedStringMap(cm.Object, data, "data"); err != nil {
			return nil, fmt.Errorf("ConfigMap %s/%s: write merged chain: %w", cm.GetNamespace(), cm.GetName(), err)
		}
		// Re-stamp the surviving Deployment: configureSharedExtProc hashed
		// the pre-merge extproc.yaml, so without this the merged chain never
		// rolls the pod.
		mergedHash := sha256.Sum256([]byte(merged))
		for j := range resources {
			deploy := &resources[j]
			if deploy.GetKind() != "Deployment" || deploy.GetName() != PayloadProcessingDeploymentName(tenantID) {
				continue
			}
			if err := unstructured.SetNestedField(deploy.Object, hex.EncodeToString(mergedHash[:]),
				"spec", "template", "metadata", "annotations", extprocConfigHashAnnotation); err != nil {
				return nil, fmt.Errorf("Deployment %s/%s: write merged config hash: %w", deploy.GetNamespace(), deploy.GetName(), err)
			}
			if err := mountServiceCA(deploy); err != nil {
				return nil, fmt.Errorf("Deployment %s/%s: mount service CA: %w", deploy.GetNamespace(), deploy.GetName(), err)
			}
			break
		}
	}
	if preFilters == nil {
		return nil, fmt.Errorf("no plugin ConfigMap chain pair found to collapse for tenant %s", tenantID)
	}

	// 2. Drop the pre-auth Deployment and everything else named for it.
	prePrefix := PayloadPreProcessingDeploymentName(tenantID)
	kept := make([]unstructured.Unstructured, 0, len(resources))
	for i := range resources {
		if strings.HasPrefix(resources[i].GetName(), prePrefix) {
			continue
		}
		kept = append(kept, resources[i])
	}
	resources = kept

	// 3. EnvoyFilter: remove the ipp-pre ext_proc INSERT patches so a single
	// ext_proc filter (ipp, → payload-processing) remains.
	for i := range resources {
		ef := &resources[i]
		if ef.GetKind() != "EnvoyFilter" {
			continue
		}
		patches, found, err := unstructured.NestedSlice(ef.Object, "spec", "configPatches")
		if err != nil || !found {
			continue
		}
		keptPatches := patches[:0]
		for _, p := range patches {
			pm, ok := p.(map[string]interface{})
			if !ok {
				keptPatches = append(keptPatches, p)
				continue
			}
			name, _, _ := unstructured.NestedString(pm, "patch", "value", "name")
			if name == "envoy.filters.http.ext_proc.ipp-pre" {
				continue
			}
			keptPatches = append(keptPatches, pm)
		}
		if err := unstructured.SetNestedSlice(ef.Object, keptPatches, "spec", "configPatches"); err != nil {
			return nil, fmt.Errorf("EnvoyFilter %s/%s: write patches: %w", ef.GetNamespace(), ef.GetName(), err)
		}
	}
	return resources, nil
}

// mergePraxisChains concatenates the pre chain's filters ahead of the post
// chain's filters in one document (post's server block wins: metrics_auth
// off, matching the post-auth ServiceAccount that holds no API permissions).
// Returns the merged YAML and the pre filters it moved.
func mergePraxisChains(pre, post string) (string, []interface{}, error) {
	var preDoc, postDoc map[string]interface{}
	if err := yaml.Unmarshal([]byte(pre), &preDoc); err != nil {
		return "", nil, fmt.Errorf("parse pre chain: %w", err)
	}
	if err := yaml.Unmarshal([]byte(post), &postDoc); err != nil {
		return "", nil, fmt.Errorf("parse post chain: %w", err)
	}
	preChains, _, _ := unstructured.NestedSlice(preDoc, "filter_chains")
	var preFilters []interface{}
	for _, c := range preChains {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		fs, _, _ := unstructured.NestedSlice(cm, "filters")
		preFilters = append(preFilters, fs...)
	}
	if len(preFilters) == 0 {
		return "", nil, errors.New("pre chain has no filters")
	}
	postChains, found, err := unstructured.NestedSlice(postDoc, "filter_chains")
	if err != nil || !found {
		return "", nil, fmt.Errorf("post chain has no filter_chains: %v", err)
	}
	first, ok := postChains[0].(map[string]interface{})
	if !ok {
		return "", nil, errors.New("post chain[0] is not a map")
	}
	postFilters, _, _ := unstructured.NestedSlice(first, "filters")
	if err := unstructured.SetNestedSlice(first, append(preFilters, postFilters...), "filters"); err != nil {
		return "", nil, fmt.Errorf("write merged filters: %w", err)
	}
	// NestedSlice returns a copy: write the modified chain back through the
	// parent document, or the merge silently vanishes at marshal time.
	newChains := append([]interface{}{first}, postChains[1:]...)
	if err := unstructured.SetNestedSlice(postDoc, newChains, "filter_chains"); err != nil {
		return "", nil, fmt.Errorf("write merged chains: %w", err)
	}
	// praxis 0.8.0 compatibility (see patchPreChainForPraxis08): the merged
	// chain carries the policy filter, so the server needs the unbounded-body
	// opt-in clamped to its cap.
	if _, ok := postDoc["insecure_options"]; !ok {
		postDoc["insecure_options"] = map[string]interface{}{"allow_unbounded_body": true}
	}
	out, err := yaml.Marshal(postDoc)
	if err != nil {
		return "", nil, fmt.Errorf("marshal merged chain: %w", err)
	}
	return string(out), preFilters, nil
}

// gatewayNamespaceOf returns the namespace of the first Deployment in the
// rendered set (ext_proc Deployments land in the gateway namespace).
func gatewayNamespaceOf(resources []unstructured.Unstructured) string {
	for i := range resources {
		if resources[i].GetKind() == "Deployment" && resources[i].GetNamespace() != "" {
			return resources[i].GetNamespace()
		}
	}
	return ""
}

// ppeEgressRule is the egress rule the PPE api-key directory needs: TCP 8443
// to maas-api's namespace.
func ppeEgressRule() map[string]interface{} {
	return map[string]interface{}{
		"to": []interface{}{
			map[string]interface{}{
				"namespaceSelector": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"kubernetes.io/metadata.name": "odh-ai-gateway-infra",
					},
				},
			},
		},
		"ports": []interface{}{
			map[string]interface{}{"port": int64(8443), "protocol": "TCP"},
		},
	}
}

// configurePPENetworkPolicy appends the maas-api egress rule to the rendered
// ext_proc NetworkPolicy. The controller re-renders the policy on every
// reconcile, so this must live in the render path, not a live patch.
func configurePPENetworkPolicy(resources []unstructured.Unstructured, gatewayNamespace string) {
	for i := range resources {
		np := &resources[i]
		if np.GetKind() != "NetworkPolicy" || np.GetNamespace() != gatewayNamespace {
			continue
		}
		egress, found, err := unstructured.NestedSlice(np.Object, "spec", "egress")
		if err != nil || !found {
			continue
		}
		rule := ppeEgressRule()
		dup := false
		for _, existing := range egress {
			m, ok := existing.(map[string]interface{})
			if !ok {
				continue
			}
			ports, _, _ := unstructured.NestedSlice(m, "ports")
			for _, p := range ports {
				pm, ok := p.(map[string]interface{})
				if ok {
					port, _, _ := unstructured.NestedInt64(pm, "port")
					if port == 8443 {
						dup = true
					}
				}
			}
		}
		if dup {
			continue
		}
		egress = append(egress, rule)
		if err := unstructured.SetNestedSlice(np.Object, egress, "spec", "egress"); err == nil {
			// Label the ext_proc pods so maas-api's ingress policy (which
			// admits the Envoy gateway pods) also admits the directory calls.
			_ = labelDeploymentsAsGateway(resources, gatewayNamespace)
		}
	}
}

// labelDeploymentsAsGateway adds the gateway pod label the maas-api ingress
// NetworkPolicy matches on to the ext_proc Deployments, so their api-key
// directory calls are admitted.
func labelDeploymentsAsGateway(resources []unstructured.Unstructured, gatewayNamespace string) error {
	for i := range resources {
		deploy := &resources[i]
		if deploy.GetKind() != "Deployment" || deploy.GetNamespace() != gatewayNamespace {
			continue
		}
		labels, found, err := unstructured.NestedStringMap(deploy.Object, "spec", "template", "metadata", "labels")
		if err != nil || !found {
			continue
		}
		if v, ok := labels["gateway.networking.k8s.io/gateway-name"]; ok && v != "" {
			continue
		}
		if labels == nil {
			labels = map[string]string{}
		}
		labels["gateway.networking.k8s.io/gateway-name"] = "maas-default-gateway"
		if err := unstructured.SetNestedStringMap(deploy.Object, labels, "spec", "template", "metadata", "labels"); err != nil {
			return err
		}
	}
	return nil
}

// stampPreDeploymentHash rolls the pre-auth Deployment when the PPE config
// changes. The pre-auth chain loads its config once at startup, so a ConfigMap
// change needs a pod roll; the shared post chain has the same mechanism in
// configureSharedExtProc.
func stampPreDeploymentHash(resources []unstructured.Unstructured, tenantID, maasAPIURL string) error {
	preDeploy := PayloadPreProcessingDeploymentName(tenantID)
	configHash := sha256.Sum256([]byte(ppePolicyDocument(maasAPIURL, tenantID)))
	for i := range resources {
		deploy := &resources[i]
		if deploy.GetKind() != "Deployment" || deploy.GetName() != preDeploy {
			continue
		}
		if err := unstructured.SetNestedField(deploy.Object, hex.EncodeToString(configHash[:]),
			"spec", "template", "metadata", "annotations", extprocConfigHashAnnotation); err != nil {
			return fmt.Errorf("Deployment %s/%s: write PPE config hash: %w", deploy.GetNamespace(), deploy.GetName(), err)
		}
		break
	}
	return nil
}

// patchPreChainForPraxis08 adapts the vendored pre-auth chain to praxis
// 0.8.0's stricter pipeline validation: an explicit max_body_bytes on the
// llmisvc resolver and server-level allow_unbounded_body (clamped to the
// server max_body_bytes cap). Returns the patched YAML and whether anything
// changed.
func patchPreChainForPraxis08(pre string, maxBodyBytes int64) (string, bool) {
	changed := false
	resolver := "      - filter: llmisvc_model_provider_resolver\n"
	if strings.Contains(pre, resolver) && !strings.Contains(pre, resolver+"        max_body_bytes:") {
		pre = strings.Replace(pre, resolver,
			fmt.Sprintf("%s        max_body_bytes: %d\n", resolver, maxBodyBytes), 1)
		changed = true
	}
	// Match the key, not the substring: the vendored chain carries a comment
	// mentioning allow_unbounded_body (RHOAIENG-102139 note), which must not
	// suppress this append.
	if !strings.Contains(pre, "\ninsecure_options:") {
		pre += fmt.Sprintf("\ninsecure_options:\n  # praxis 0.8.0: the policy filter's nested pipeline trips the\n  # unbounded-StreamBuffer check; allow and clamp to the server cap.\n  allow_unbounded_body: true\n")
		changed = true
	}
	return pre, changed
}

// ppePolicyDocument renders the POC PPE policy document. Shape follows the
// upstream fixtures (tests/integration/fixtures/rate-limit-policy.yaml in
// praxis-proxy/praxis): an identity/api-key resolver over maas-api's HTTP
// directory, a post-identity in-process limiter keyed on the subscription,
// and route-scoped authorization.
func ppePolicyDocument(maasAPIURL, tenantID string) string {
	var b strings.Builder
	b.WriteString("# Rendered by ai-gateway-controller (PPE POC). Tenant: ")
	b.WriteString(tenantID)
	b.WriteString("\nengine_settings:\n  dispatch: policy\n\n")
	b.WriteString("plugins:\n")
	b.WriteString("  - name: maas-api-keys\n")
	b.WriteString("    kind: identity/api-key\n")
	b.WriteString("    hooks: [identity.resolve]\n")
	b.WriteString("    on_error: fail\n")
	b.WriteString("    capabilities: [perform_http, read_headers]\n")
	b.WriteString("    config:\n")
	b.WriteString("      credential:\n")
	b.WriteString("        kind: header\n")
	b.WriteString("        name: Authorization\n")
	b.WriteString("      prefix: \"Bearer \"\n")
	b.WriteString("      provider:\n")
	b.WriteString("        kind: http\n")
	b.WriteString(fmt.Sprintf("        url: %s\n", maasAPIURL))
	b.WriteString("        timeout_secs: 5\n")
	// record_map maps record fields onto the typed identity slots
	// (subject.id/permissions/roles/teams only). Unconsumed record fields
	// (subscription, username, tenant) land in the claims bag as claim.*,
	// which the rate-limit binding below reads.
	b.WriteString("      record_map:\n")
	b.WriteString("        subject:\n")
	b.WriteString("          id: userId\n")
	b.WriteString("          teams: groups\n")
	b.WriteString("\n")
	b.WriteString("  - name: per-subscription-limits\n")
	b.WriteString("    kind: ratelimit/limitador\n")
	b.WriteString("    hooks: [ratelimit.post_identity]\n")
	b.WriteString("    capabilities: [read_subject]\n")
	b.WriteString("    config:\n")
	b.WriteString("      namespace: maas-subscription\n")
	b.WriteString("      bindings:\n")
	// Keyed on subject.id: the api-key record's unconsumed fields do not
	// surface as claim.* attributes for limitador bindings
	// (ratelimit.missing_attribute), and subject.id matches the upstream
	// rate-limit fixture's shape.
	b.WriteString("        subject_id: subject.id\n")
	b.WriteString("      limits:\n")
	// Same default the validation flow trips (~4 requests): the free
	// simulator subscription's windowed request limit. Single-replica only;
	// counters are per instance (ADR MAJOR GAP 3).
	b.WriteString("        - max: 4\n")
	b.WriteString("          seconds: 60\n")
	b.WriteString("          variables: [subject_id]\n")
	b.WriteString("\n")
	b.WriteString("global:\n")
	b.WriteString("  authentication:\n")
	b.WriteString("    - maas-api-keys\n")
	b.WriteString("  rate_limits:\n")
	b.WriteString("    - per-subscription-limits\n")
	b.WriteString("\n")
	b.WriteString("routes:\n")
	// maas-api keeps its own OpenShift-token validation; PPE must not sit in
	// front of it (also avoids the directory calling itself through the
	// gateway).
	b.WriteString("  - http:\n")
	b.WriteString("      path_prefix: /maas-api\n")
	b.WriteString("    authorization:\n")
	b.WriteString("      pre_invocation:\n")
	b.WriteString("        - \"allow\"\n")
	b.WriteString("  - http:\n")
	b.WriteString("      path_prefix: /\n")
	b.WriteString("    authorization:\n")
	b.WriteString("      pre_invocation:\n")
	b.WriteString("        - \"require(authenticated)\"\n")
	return b.String()
}

// mountServiceCA gives the merged-chain Deployment the cluster service CA
// and points the subrequest transport's root store at it (rustls-native-certs
// honors SSL_CERT_FILE). PPE's api-key directory calls maas-api over HTTPS
// with a service-CA-signed cert, which is not in the default trust store.
// The service-ca-bundle ConfigMap is provisioned by the service CA operator
// in every namespace it is requested in; openshift-ingress carries one.
func mountServiceCA(deploy *unstructured.Unstructured) error {
	volumes, _, err := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "volumes")
	if err != nil {
		return err
	}
	hasVol := false
	for _, v := range volumes {
		if vm, ok := v.(map[string]interface{}); ok {
			if name, _, _ := unstructured.NestedString(vm, "name"); name == "ppe-service-ca" {
				hasVol = true
			}
		}
	}
	if !hasVol {
		volumes = append(volumes, map[string]interface{}{
			"name":      "ppe-service-ca",
			"configMap": map[string]interface{}{"name": "service-ca-bundle"},
		})
		if err := unstructured.SetNestedSlice(deploy.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
			return err
		}
	}
	// containers is a list: read it, mutate the first element's maps, write
	// the list back (NestedSlice cannot index into list items).
	containers, _, err := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	if err != nil || len(containers) == 0 {
		return fmt.Errorf("read containers: %v", err)
	}
	container, ok := containers[0].(map[string]interface{})
	if !ok {
		return errors.New("containers[0] is not a map")
	}
	mounts, _, err := unstructured.NestedSlice(container, "volumeMounts")
	if err != nil {
		return err
	}
	hasMount := false
	for _, m := range mounts {
		if mm, ok := m.(map[string]interface{}); ok {
			if name, _, _ := unstructured.NestedString(mm, "name"); name == "ppe-service-ca" {
				hasMount = true
			}
		}
	}
	if !hasMount {
		mounts = append(mounts, map[string]interface{}{
			"name": "ppe-service-ca", "mountPath": "/etc/ppe-ca", "readOnly": true,
		})
		if err := unstructured.SetNestedSlice(container, mounts, "volumeMounts"); err != nil {
			return err
		}
	}
	env, _, err := unstructured.NestedSlice(container, "env")
	if err != nil {
		return err
	}
	hasEnv := false
	for _, e := range env {
		if em, ok := e.(map[string]interface{}); ok {
			if name, _, _ := unstructured.NestedString(em, "name"); name == "SSL_CERT_FILE" {
				hasEnv = true
			}
		}
	}
	if !hasEnv {
		env = append(env, map[string]interface{}{
			"name": "SSL_CERT_FILE", "value": "/etc/ppe-ca/service-ca.crt",
		})
		if err := unstructured.SetNestedSlice(container, env, "env"); err != nil {
			return err
		}
	}
	return unstructured.SetNestedSlice(deploy.Object, containers, "spec", "template", "spec", "containers")
}

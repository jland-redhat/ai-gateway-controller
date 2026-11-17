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
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
func configureExtProcPolicyEngine(resources []unstructured.Unstructured, maasAPIURL, maasAPIHost, tenantID string) error {
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
		if strings.Contains(pre, "filter: policy") {
			// Already injected (idempotent re-reconcile): still rewrite the
			// policy document so annotation/render changes propagate.
			data[PPDPolicyConfigMapKey] = ppePolicyDocument(maasAPIURL, tenantID)
			if err := unstructured.SetNestedStringMap(cm.Object, data, "data"); err != nil {
				return fmt.Errorf("ConfigMap %s/%s: write data: %w", cm.GetNamespace(), cm.GetName(), err)
			}
			patched++
			continue
		}
		data["pre-extproc.yaml"] = pre + fmt.Sprintf(preAuthPolicyFilterYAML, maasAPIHost)
		data[PPDPolicyConfigMapKey] = ppePolicyDocument(maasAPIURL, tenantID)
		if err := unstructured.SetNestedStringMap(cm.Object, data, "data"); err != nil {
			return fmt.Errorf("ConfigMap %s/%s: write data: %w", cm.GetNamespace(), cm.GetName(), err)
		}
		patched++
	}
	if patched == 0 {
		return fmt.Errorf("no plugin ConfigMap pre-extproc.yaml found to inject the policy filter for tenant %s", tenantID)
	}
	return nil
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
	b.WriteString("        subscription: claim.subscription\n")
	b.WriteString("      limits:\n")
	// Same default the validation flow trips (~4 requests): the free
	// simulator subscription's windowed request limit. Single-replica only;
	// counters are per instance (ADR MAJOR GAP 3).
	b.WriteString("        - max: 4\n")
	b.WriteString("          seconds: 60\n")
	b.WriteString("          variables: [subscription]\n")
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

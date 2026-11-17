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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestTenantUsesPPE(t *testing.T) {
	mtc := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if TenantUsesPPE(mtc) {
		t.Fatal("absent annotation must not select PPE")
	}
	if TenantUsesPPE(nil) {
		t.Fatal("nil MaasTenantConfig must not select PPE")
	}
	mtc.SetAnnotations(map[string]string{AnnotationPolicyEngine: "rhcl"})
	if TenantUsesPPE(mtc) {
		t.Fatal("rhcl value must not select PPE")
	}
	mtc.SetAnnotations(map[string]string{AnnotationPolicyEngine: PolicyEnginePPE})
	if !TenantUsesPPE(mtc) {
		t.Fatal("ppe value must select PPE")
	}
}

func TestConfigureExtProcPolicyEngine(t *testing.T) {
	cm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": PayloadProcessingPluginsConfigMapName},
		"data": map[string]interface{}{
			"pre-extproc.yaml": "server:\n  grpc_address: \"0.0.0.0:9004\"\nfilter_chains:\n  - name: bbr\n    filters:\n      - filter: headers\n",
			"extproc.yaml":     "server:\n",
		},
	}}
	resources := []unstructured.Unstructured{*cm}
	if err := configureExtProcPolicyEngine(resources, "http://maas-api.infra.svc.cluster.local:8080/internal/v1/api-keys/validate", "maas-api.infra.svc.cluster.local", "t1"); err != nil {
		t.Fatalf("configureExtProcPolicyEngine: %v", err)
	}
	data, _, err := unstructured.NestedStringMap(resources[0].Object, "data")
	if err != nil {
		t.Fatal(err)
	}
	pre := data["pre-extproc.yaml"]
	if !strings.Contains(pre, "- filter: policy") {
		t.Error("pre-auth chain must gain the policy filter")
	}
	if !strings.Contains(pre, "config_path: "+PPDPolicyConfigPath) {
		t.Error("policy filter must point at the rendered policy document")
	}
	if !strings.Contains(pre, "trusted_private_endpoints:") || !strings.Contains(pre, "maas-api.infra.svc.cluster.local") {
		t.Error("policy filter must allow-list the maas-api host")
	}
	policy := data[PPDPolicyConfigMapKey]
	for _, want := range []string{
		"kind: identity/api-key",
		"kind: http",
		"/internal/v1/api-keys/validate",
		"kind: ratelimit/limitador",
		"path_prefix: /maas-api",
		"require(authenticated)",
	} {
		if !strings.Contains(policy, want) {
			t.Errorf("policy document missing %q", want)
		}
	}
	// Idempotent: a second pass must not duplicate the filter.
	if err := configureExtProcPolicyEngine(resources, "http://maas-api.infra.svc.cluster.local:8080/internal/v1/api-keys/validate", "maas-api.infra.svc.cluster.local", "t1"); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	data, _, _ = unstructured.NestedStringMap(resources[0].Object, "data")
	if got := strings.Count(data["pre-extproc.yaml"], "- filter: policy"); got != 1 {
		t.Errorf("policy filter injected %d times, want 1", got)
	}
}

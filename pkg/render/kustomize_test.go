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

package render

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestNormalizeJSONTypesConvertsIntToInt64(t *testing.T) {
	obj := map[string]any{
		"replicas": 3,
		"nested": map[string]any{
			"port": 9004,
		},
		"list": []any{1, 2, map[string]any{"n": 5}},
	}

	normalizeJSONTypes(obj)

	if _, ok := obj["replicas"].(int64); !ok {
		t.Fatalf("replicas = %T, want int64", obj["replicas"])
	}

	nested, ok := obj["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested = %T, want map[string]any", obj["nested"])
	}
	if _, ok := nested["port"].(int64); !ok {
		t.Fatalf("nested.port = %T, want int64", nested["port"])
	}

	list, ok := obj["list"].([]any)
	if !ok {
		t.Fatalf("list = %T, want []any", obj["list"])
	}
	if _, ok := list[0].(int64); !ok {
		t.Fatalf("list[0] = %T, want int64", list[0])
	}
	listItem, ok := list[2].(map[string]any)
	if !ok {
		t.Fatalf("list[2] = %T, want map[string]any", list[2])
	}
	if _, ok := listItem["n"].(int64); !ok {
		t.Fatalf("list[2].n = %T, want int64", listItem["n"])
	}
}

// TestRenderKustomizeBuildsVendoredOverlay is a smoke test against the real
// vendored manifest tree (hack/scripts/get-manifests.sh). It is skipped, not
// failed, when the manifests have not been fetched yet (e.g. a fresh clone
// before "make get-manifests").
func TestRenderKustomizeBuildsVendoredOverlay(t *testing.T) {
	const manifestPath = "../../config/manifests/praxis-extproc/overlays/odh"

	resources, err := Build(manifestPath)
	if err != nil {
		t.Skipf("vendored manifests not present at %s; run hack/scripts/get-manifests.sh first: %v", manifestPath, err)
	}

	wantKinds := map[string]int{
		"ServiceAccount":     1,
		"ClusterRole":        1,
		"ClusterRoleBinding": 1,
		"ConfigMap":          1,
		"Service":            2,
		"Deployment":         2,
		"DestinationRule":    2,
		"EnvoyFilter":        1,
		"NetworkPolicy":      1,
	}
	gotKinds := map[string]int{}
	for _, r := range resources {
		gotKinds[r.GetKind()]++
	}
	for kind, want := range wantKinds {
		if gotKinds[kind] != want {
			t.Errorf("kind %s: got %d resources, want %d (full count map: %v)", kind, gotKinds[kind], want, gotKinds)
		}
	}
}

func TestRenderedExtProcPreservesBufferedMaaSAndAddsHeaderPhaseExternalModel(t *testing.T) {
	const manifestPath = "../../config/manifests/praxis-extproc/overlays/odh"

	resources, err := Build(manifestPath)
	if err != nil {
		t.Skipf("vendored manifests not present at %s: %v", manifestPath, err)
	}

	var envoyFilter *unstructured.Unstructured
	for i := range resources {
		if resources[i].GetKind() == "EnvoyFilter" {
			envoyFilter = &resources[i]
			break
		}
	}
	if envoyFilter == nil {
		t.Fatal("rendered overlay has no EnvoyFilter")
	}

	patches, found, err := unstructured.NestedSlice(envoyFilter.Object, "spec", "configPatches")
	if err != nil || !found {
		t.Fatalf("EnvoyFilter configPatches missing: found=%v err=%v", found, err)
	}
	var postAuth, externalModel, preAuth, externalModelDefaultDisabled int
	for _, raw := range patches {
		patch, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		value, ok := patch["patch"].(map[string]any)
		if !ok {
			continue
		}
		if patch["applyTo"] == "VIRTUAL_HOST" {
			if disabled, found, _ := unstructured.NestedBool(patch, "patch", "value", "typed_per_filter_config", "envoy.filters.http.ext_proc.external-model", "disabled"); found && disabled {
				externalModelDefaultDisabled++
			}
			continue
		}
		resource, ok := value["value"].(map[string]any)
		if !ok || (resource["name"] != "envoy.filters.http.ext_proc.ipp" && resource["name"] != "envoy.filters.http.ext_proc.external-model") {
			continue
		}
		mode, found, err := unstructured.NestedString(resource, "typed_config", "processing_mode", "request_body_mode")
		if err != nil || !found {
			t.Fatalf("post-auth processing mode missing: found=%v err=%v", found, err)
		}
		if resource["name"] == "envoy.filters.http.ext_proc.ipp" {
			if mode != "BUFFERED" {
				t.Fatalf("shared post-auth request_body_mode = %q, want BUFFERED", mode)
			}
			postAuth++
		} else {
			if mode != "NONE" {
				t.Fatalf("ExternalModel request_body_mode = %q, want NONE", mode)
			}
			externalModel++
		}
	}

	for _, raw := range patches {
		patch, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		value, ok := patch["patch"].(map[string]any)
		if !ok {
			continue
		}
		resource, ok := value["value"].(map[string]any)
		if !ok || resource["name"] != "envoy.filters.http.ext_proc.ipp-pre" {
			continue
		}
		mode, found, err := unstructured.NestedString(resource, "typed_config", "processing_mode", "request_body_mode")
		if err != nil || !found {
			t.Fatalf("pre-auth processing mode missing: found=%v err=%v", found, err)
		}
		if mode != "BUFFERED" {
			t.Fatalf("pre-auth request_body_mode = %q, want BUFFERED", mode)
		}
		preAuth++
	}

	if postAuth == 0 || externalModel == 0 || preAuth == 0 || externalModelDefaultDisabled == 0 {
		t.Fatalf("expected buffered post-auth, header-phase ExternalModel, pre-auth, and default-disabled patches, got post=%d external=%d pre=%d disabled=%d", postAuth, externalModel, preAuth, externalModelDefaultDisabled)
	}
}

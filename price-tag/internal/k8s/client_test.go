package k8s

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/redhat-et/pricetag-metering/internal/config"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// TestOverlayDocParsing verifies the routing-overlay JSON structure the
// intelligent_route gateways deploy (praxis-routing-openai /
// praxis-routing-unified ConfigMaps) parses into overlayDoc correctly.
func TestOverlayDocParsing(t *testing.T) {
	// Shape captured from the live prod overlays (2026-10-21).
	raw := `{
		"selection_policy": {"mode": "deterministic"},
		"candidates": [
			{
				"kind": "inference_model",
				"name": "rits/deepseek-ai/DeepSeek-V4.1-Flash-1017",
				"site": "enmaas",
				"cluster": "cb-glm",
				"catalog": ["openai"],
				"owned_by": "curvebender"
			},
			{
				"kind": "inference_model",
				"name": "claude-haiku-5-5",
				"site": "enmaas",
				"cluster": "vertex",
				"provider": "vertex",
				"target_model": "vertex/claude-haiku-5-5",
				"paths": ["/v1/messages", "/v1/messages/count_tokens"],
				"catalog": ["anthropic"],
				"owned_by": "vertex"
			}
		]
	}`
	var doc overlayDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("parse overlay JSON: %v", err)
	}
	if len(doc.Candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(doc.Candidates))
	}
	ds := doc.Candidates[0]
	if ds.Name != "rits/deepseek-ai/DeepSeek-V4.1-Flash-1017" {
		t.Errorf("candidate[0].Name = %q", ds.Name)
	}
	if ds.Cluster != "cb-glm" {
		t.Errorf("candidate[0].Cluster = %q, want cb-glm", ds.Cluster)
	}
	if ds.OwnedBy != "curvebender" {
		t.Errorf("candidate[0].OwnedBy = %q, want curvebender", ds.OwnedBy)
	}
	haiku := doc.Candidates[1]
	if haiku.Provider != "vertex" {
		t.Errorf("candidate[1].Provider = %q, want vertex", haiku.Provider)
	}
	if len(haiku.Catalog) != 1 || haiku.Catalog[0] != "anthropic" {
		t.Errorf("candidate[1].Catalog = %v, want [anthropic]", haiku.Catalog)
	}
}

// TestOverlayDocParsingEmpty ensures a missing or empty candidates array
// parses without error (the overlay reader skips such CMs silently).
func TestOverlayDocParsingEmpty(t *testing.T) {
	for _, raw := range []string{`{}`, `{"candidates": []}`, `{"candidates": null}`} {
		var doc overlayDoc
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if len(doc.Candidates) != 0 {
			t.Errorf("%q: want 0 candidates, got %d", raw, len(doc.Candidates))
		}
	}
}

func fakeClient(t *testing.T, cfg config.Kubernetes, objects ...runtime.Object) *Client {
	t.Helper()
	return &Client{
		client: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(),
			map[schema.GroupVersionResource]string{
				{Group: "", Version: "v1", Resource: "configmaps"}:                "ConfigMapList",
				{Group: "example.com", Version: "v1", Resource: "externalmodels"}: "ExternalModelList",
			},
			objects...,
		),
		namespace: cfg.Namespace,
		cfg:       cfg,
	}
}

func testConfigMap(name, namespace, key, value string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"data": map[string]interface{}{key: value},
	}}
}

const legacyCatalogYAML = `filter_chains:
- name: unified
  filters:
  - filter: model_catalog
    models:
    - id: legacy-model
      owned_by: legacy
`

const routingOverlayJSON = `{"candidates":[
  {"name":"legacy-model","cluster":"cb-glm","owned_by":"curvebender"},
  {"name":"overlay-model","cluster":"cb-glm","owned_by":"curvebender"}
]}`

func modelNames(models []ModelInfo) []string {
	names := make([]string, 0, len(models))
	for _, model := range models {
		names = append(names, model.Name)
	}
	sort.Strings(names)
	return names
}

func TestListModelsMergesOverlaysWithConfigSource(t *testing.T) {
	cfg := config.Kubernetes{
		Namespace:                "test",
		PipelineConfigMap:        "praxis-config",
		PipelineConfigMapKey:     "config.yaml",
		RoutingOverlayConfigMaps: []string{"praxis-routing-openai"},
	}
	client := fakeClient(t, cfg,
		testConfigMap("praxis-config", "test", "config.yaml", legacyCatalogYAML),
		testConfigMap("praxis-routing-openai", "test", "routing-overlay.json", routingOverlayJSON),
	)
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if got, want := strings.Join(modelNames(models), ","), "legacy-model,overlay-model"; got != want {
		t.Fatalf("models = %s, want %s", got, want)
	}
}

func TestListModelsMergesOverlaysWithCRDSource(t *testing.T) {
	model := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "example.com/v1",
		"kind":       "ExternalModel",
		"metadata": map[string]interface{}{
			"name":      "crd-model",
			"namespace": "test",
		},
		"spec": map[string]interface{}{
			"provider":    "legacy",
			"targetModel": "crd-model",
		},
	}}
	cfg := config.Kubernetes{
		Namespace:                "test",
		ModelGroup:               "example.com",
		ModelVersion:             "v1",
		ModelResource:            "externalmodels",
		RoutingOverlayConfigMaps: []string{"praxis-routing-openai"},
	}
	client := fakeClient(t, cfg,
		model,
		testConfigMap("praxis-routing-openai", "test", "routing-overlay.json", routingOverlayJSON),
	)
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if got, want := strings.Join(modelNames(models), ","), "crd-model,legacy-model,overlay-model"; got != want {
		t.Fatalf("models = %s, want %s", got, want)
	}
}

func TestListModelsUsesOverlaysWhenLegacyConfigUnavailable(t *testing.T) {
	cfg := config.Kubernetes{
		Namespace:                "test",
		PipelineConfigMap:        "missing-config",
		PipelineConfigMapKey:     "config.yaml",
		RoutingOverlayConfigMaps: []string{"praxis-routing-openai"},
	}
	client := fakeClient(t, cfg,
		testConfigMap("praxis-routing-openai", "test", "routing-overlay.json", routingOverlayJSON),
	)
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels should tolerate a missing legacy source: %v", err)
	}
	if got, want := strings.Join(modelNames(models), ","), "legacy-model,overlay-model"; got != want {
		t.Fatalf("models = %s, want %s", got, want)
	}
}

func TestListModelsReturnsSourceErrorWithoutAnyModels(t *testing.T) {
	cfg := config.Kubernetes{
		Namespace:            "test",
		PipelineConfigMap:    "missing-config",
		PipelineConfigMapKey: "config.yaml",
	}
	client := fakeClient(t, cfg)
	if _, err := client.ListModels(context.Background()); err == nil {
		t.Fatal("ListModels must return the source error when no fallback models exist")
	}
}

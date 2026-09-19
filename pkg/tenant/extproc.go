/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package tenant

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
)

const extprocCredentialDir = "/etc/praxis/credentials" //nolint:gosec // fixed non-secret mount path

type extprocCredential struct {
	name      string
	namespace string
	key       string
	file      string
}

// configureExternalModelExtProc completes the dedicated ExternalModel ExtProc
// render with tenant-specific routing and credential references. The upstream
// MaaS/KServe post-auth workload is intentionally not modified. Secret values
// are never read: kubelet projects the referenced Secret into this workload and
// credential_inject reads the mounted file at request time.
func configureExternalModelExtProc(resources []unstructured.Unstructured, namespace string, providers []v1alpha1.ExternalProvider) error {
	credentials, err := extprocCredentials(namespace, providers)
	if err != nil {
		return err
	}
	config := extprocPostAuthConfig(providers, credentials)

	var configMap *unstructured.Unstructured
	var deployment *unstructured.Unstructured
	for i := range resources {
		resource := &resources[i]
		if configMap == nil && resource.GetNamespace() == namespace && resource.GetKind() == "ConfigMap" && strings.HasPrefix(resource.GetName(), PayloadProcessingExternalModelName+"-plugins") {
			configMap = resource
		}
		if deployment == nil && resource.GetNamespace() == namespace && resource.GetKind() == "Deployment" && strings.HasPrefix(resource.GetName(), PayloadProcessingExternalModelName) {
			deployment = resource
		}
	}
	if configMap == nil || deployment == nil {
		return errors.New("post-auth ExtProc resources are missing the plugin ConfigMap or Deployment")
	}
	data, found, err := unstructured.NestedStringMap(configMap.Object, "data")
	if err != nil {
		return fmt.Errorf("read post-auth ExtProc ConfigMap data: %w", err)
	}
	if !found {
		return errors.New("post-auth ExtProc ConfigMap data is missing")
	}
	data["extproc.yaml"] = config
	if err := unstructured.SetNestedStringMap(configMap.Object, data, "data"); err != nil {
		return fmt.Errorf("write post-auth ExtProc ConfigMap data: %w", err)
	}
	configHash := sha256.Sum256([]byte(config))
	if err := unstructured.SetNestedField(deployment.Object, hex.EncodeToString(configHash[:]), "spec", "template", "metadata", "annotations", "ai-gateway-controller.opendatahub.io/extproc-config-sha256"); err != nil {
		return fmt.Errorf("write post-auth ExtProc config hash: %w", err)
	}

	if len(credentials) == 0 {
		return nil
	}
	volumes, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "volumes")
	if err != nil {
		return fmt.Errorf("read post-auth ExtProc volumes: %w", err)
	}
	projectedSources := make([]any, 0, len(credentials))
	for _, credential := range credentials {
		projectedSources = append(projectedSources, map[string]any{
			"secret": map[string]any{
				"name":  credential.name,
				"items": []any{map[string]any{"key": credential.key, "path": strings.TrimPrefix(credential.file, extprocCredentialDir+"/")}},
			},
		})
	}
	volumes = appendOrReplaceNamedVolume(volumes, "provider-credentials", map[string]any{
		"name":      "provider-credentials",
		"projected": map[string]any{"sources": projectedSources},
	})
	if err := unstructured.SetNestedSlice(deployment.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		return fmt.Errorf("write post-auth ExtProc volumes: %w", err)
	}
	containers, found, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) == 0 {
		if err == nil {
			err = errors.New("containers are missing")
		}
		return fmt.Errorf("read post-auth ExtProc containers: %w", err)
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		return errors.New("post-auth ExtProc first container is malformed")
	}
	mounts, _, err := unstructured.NestedSlice(container, "volumeMounts")
	if err != nil {
		return fmt.Errorf("read post-auth ExtProc volume mounts: %w", err)
	}
	mounts = appendOrReplaceNamedVolume(mounts, "provider-credentials", map[string]any{
		"name": "provider-credentials", "mountPath": extprocCredentialDir, "readOnly": true,
	})
	if err := unstructured.SetNestedSlice(container, mounts, "volumeMounts"); err != nil {
		return fmt.Errorf("write post-auth ExtProc volume mounts: %w", err)
	}
	containers[0] = container
	if err := unstructured.SetNestedSlice(deployment.Object, containers, "spec", "template", "spec", "containers"); err != nil {
		return fmt.Errorf("write post-auth ExtProc containers: %w", err)
	}
	return nil
}

// extprocCredentials returns deterministic, reference-only file mappings.
func extprocCredentials(namespace string, providers []v1alpha1.ExternalProvider) ([]extprocCredential, error) {
	seen := map[string]bool{}
	credentials := make([]extprocCredential, 0, len(providers))
	for _, provider := range providers {
		if provider.Namespace != "" && provider.Namespace != namespace {
			return nil, fmt.Errorf("provider %s/%s references a cross-namespace Secret", provider.Namespace, provider.Name)
		}
		if provider.Spec.Auth.Type != "" && provider.Spec.Auth.Type != "apikey" {
			return nil, fmt.Errorf("provider %s uses unsupported ExtProc credential type %q", provider.Name, provider.Spec.Auth.Type)
		}
		name := provider.Spec.Auth.SecretRef.Name
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		key := "api-key"
		hash := sha256.Sum256([]byte(namespace + "/" + name + "/" + key))
		path := fmt.Sprintf("%s/%s-%s/%s", extprocCredentialDir, name, hex.EncodeToString(hash[:])[:12], key)
		credentials = append(credentials, extprocCredential{name: name, namespace: namespace, key: key, file: path})
	}
	sort.Slice(credentials, func(i, j int) bool { return credentials[i].name < credentials[j].name })
	return credentials, nil
}

func extprocPostAuthConfig(providers []v1alpha1.ExternalProvider, credentials []extprocCredential) string {
	clusters := make([]string, 0, len(providers))
	for _, provider := range providers {
		clusters = append(clusters, "provider-"+provider.Name)
	}
	sort.Strings(clusters)
	var b strings.Builder
	b.WriteString("server:\n  grpc_address: \"0.0.0.0:9004\"\n  health_address: \"0.0.0.0:50052\"\n")
	b.WriteString("  metrics_address: \"0.0.0.0:9090\"\n  tls:\n    mode: self_signed\n\n")
	b.WriteString("filter_chains:\n  - name: post-auth\n    filters:\n      - filter: intelligent_route\n")
	b.WriteString("        overlay_file: /etc/praxis/routing/routing-overlay.json\n")
	b.WriteString("        model_header: X-Gateway-Model-Name\n        provider_hop_clusters: [")
	for i, cluster := range clusters {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", cluster)
	}
	b.WriteString("]\n        reload: {enabled: true, debounce_ms: 500}\n")
	if len(credentials) > 0 {
		b.WriteString("      - filter: credential_inject\n        credentials:\n")
		for _, credential := range credentials {
			fmt.Fprintf(&b, "          - name: %s\n            namespace: %s\n", credential.name, credential.namespace)
			fmt.Fprintf(&b, "            key: %s\n            strategy: bearer_token\n            file: %s\n", credential.key, credential.file)
		}
	}
	b.WriteString("\ninsecure_options:\n  allow_unbounded_body: true\n")
	return b.String()
}

func appendOrReplaceNamedVolume(items []any, name string, value map[string]any) []any {
	for i, item := range items {
		m, ok := item.(map[string]any)
		if ok && m["name"] == name {
			items[i] = value
			return items
		}
	}
	return append(items, value)
}

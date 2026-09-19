package tenant

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/ai-gateway-controller/pkg/render"
)

//nolint:gocyclo // The test checks every namespace/ownership boundary in one rendered resource set.
func TestSplitPostAuthResourcesUsesResolvedTenantNamespace(t *testing.T) {
	requireManifests(t)
	rendered, err := render.Build(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	resources := render.PostRender(rendered, render.Params{Namespace: "gateway-system", GatewayName: "gateway", Image: "extproc:dev"})
	resources, err = Rename(resources, "tenant-a", "gateway-system")
	if err != nil {
		t.Fatal(err)
	}
	resources, err = SplitPostAuthResources(resources, "tenant-a", "gateway-system", "tenant-a")
	if err != nil {
		t.Fatal(err)
	}

	find := func(kind, name, namespace string) *unstructured.Unstructured {
		t.Helper()
		for i := range resources {
			if resources[i].GetKind() == kind && resources[i].GetName() == name && resources[i].GetNamespace() == namespace {
				return &resources[i]
			}
		}
		t.Fatalf("missing %s %s/%s", kind, namespace, name)
		return nil
	}
	find("Deployment", PayloadPreProcessingDeploymentName("tenant-a"), "gateway-system")
	postDeployment := find("Deployment", PayloadProcessingDeploymentName("tenant-a"), "tenant-a")
	if serviceAccount, _, _ := unstructured.NestedString(postDeployment.Object, "spec", "template", "spec", "serviceAccountName"); serviceAccount != PayloadProcessingPostServiceAccountName("tenant-a") {
		t.Fatalf("post-auth Deployment ServiceAccount = %q, want %q", serviceAccount, PayloadProcessingPostServiceAccountName("tenant-a"))
	}
	externalDeployment := find("Deployment", PayloadProcessingExternalModelDeploymentName("tenant-a"), "tenant-a")
	if serviceAccount, _, _ := unstructured.NestedString(externalDeployment.Object, "spec", "template", "spec", "serviceAccountName"); serviceAccount != PayloadProcessingExternalModelServiceAccountName("tenant-a") {
		t.Fatalf("ExternalModel Deployment ServiceAccount = %q, want %q", serviceAccount, PayloadProcessingExternalModelServiceAccountName("tenant-a"))
	}
	if selector, _, _ := unstructured.NestedString(externalDeployment.Object, "spec", "selector", "matchLabels", "app"); selector != PayloadProcessingExternalModelName {
		t.Fatalf("ExternalModel Deployment selector app = %q, want %q", selector, PayloadProcessingExternalModelName)
	}
	externalService := find("Service", PayloadProcessingExternalModelServiceName("tenant-a"), "tenant-a")
	if selector, _, _ := unstructured.NestedString(externalService.Object, "spec", "selector", "app"); selector != PayloadProcessingExternalModelName {
		t.Fatalf("ExternalModel Service selector app = %q, want %q", selector, PayloadProcessingExternalModelName)
	}
	externalRule := find("DestinationRule", PayloadProcessingExternalModelServiceName("tenant-a"), "gateway-system")
	externalFQDN := "payload-processing-external-model-tenant-a.tenant-a.svc.cluster.local"
	if host, _, _ := unstructured.NestedString(externalRule.Object, "spec", "host"); host != externalFQDN {
		t.Fatalf("ExternalModel DestinationRule host = %q, want %q", host, externalFQDN)
	}
	if sni, _, _ := unstructured.NestedString(externalRule.Object, "spec", "trafficPolicy", "tls", "sni"); sni != externalFQDN {
		t.Fatalf("ExternalModel DestinationRule SNI = %q, want %q", sni, externalFQDN)
	}
	find("ServiceAccount", PayloadProcessingServiceAccountName("tenant-a"), "gateway-system")
	postSA := find("ServiceAccount", PayloadProcessingPostServiceAccountName("tenant-a"), "tenant-a")
	if got, _, _ := unstructured.NestedString(postSA.Object, "metadata", "name"); got != PayloadProcessingPostServiceAccountName("tenant-a") {
		t.Fatalf("post-auth ServiceAccount = %q, want %q", got, PayloadProcessingPostServiceAccountName("tenant-a"))
	}
	find("Service", PayloadPreProcessingServiceName("tenant-a"), "gateway-system")
	find("Service", PayloadProcessingServiceName("tenant-a"), "tenant-a")
	postRule := find("DestinationRule", PayloadProcessingServiceName("tenant-a"), "gateway-system")
	postFQDN := "payload-processing-tenant-a.tenant-a.svc.cluster.local"
	preFQDN := "payload-pre-processing-tenant-a.gateway-system.svc.cluster.local"
	if host, _, _ := unstructured.NestedString(postRule.Object, "spec", "host"); host != postFQDN {
		t.Fatalf("post-auth DestinationRule host = %q, want tenant-qualified Service FQDN", host)
	}
	if sni, _, _ := unstructured.NestedString(postRule.Object, "spec", "trafficPolicy", "tls", "sni"); sni != postFQDN {
		t.Fatalf("post-auth DestinationRule SNI = %q, want tenant-qualified Service FQDN", sni)
	}
	for _, resource := range resources {
		if resource.GetKind() == "DestinationRule" && resource.GetName() == PayloadProcessingServiceName("tenant-a") && resource.GetNamespace() == "tenant-a" {
			t.Fatal("post-auth DestinationRule must remain in the Gateway namespace")
		}
	}

	gatewayConfig := find("ConfigMap", PayloadProcessingPluginsConfigMapForTenant("tenant-a"), "gateway-system")
	if data, _, _ := unstructured.NestedStringMap(gatewayConfig.Object, "data"); data["extproc.yaml"] != "" {
		t.Fatal("gateway ConfigMap must contain only the pre-auth configuration")
	}
	tenantConfig := find("ConfigMap", PayloadProcessingPluginsConfigMapForTenant("tenant-a"), "tenant-a")
	if data, _, _ := unstructured.NestedStringMap(tenantConfig.Object, "data"); data["extproc.yaml"] == "" || data["pre-extproc.yaml"] != "" {
		t.Fatal("tenant ConfigMap must contain only the post-auth configuration")
	}
	externalConfig := find("ConfigMap", PayloadProcessingExternalModelPluginsConfigMapForTenant("tenant-a"), "tenant-a")
	if data, _, _ := unstructured.NestedStringMap(externalConfig.Object, "data"); data["extproc.yaml"] == "" || data["pre-extproc.yaml"] != "" {
		t.Fatal("ExternalModel ConfigMap must contain only the dedicated post-auth configuration")
	}

	envoy := find("EnvoyFilter", PayloadProcessingEnvoyFilterName("tenant-a"), "gateway-system")
	patches, _, _ := unstructured.NestedSlice(envoy.Object, "spec", "configPatches")
	foundTenantCluster := false
	foundGatewayCluster := false
	for _, raw := range patches {
		patch, ok := raw.(map[string]any)
		if !ok || patch["applyTo"] != "CLUSTER" {
			continue
		}
		body, _ := patch["patch"].(map[string]any)
		value, _ := body["value"].(map[string]any)
		clusterName, _ := value["name"].(string)
		var wantFQDN string
		switch clusterName {
		case "payload-processing-extproc":
			wantFQDN = postFQDN
		case "payload-pre-processing-extproc":
			wantFQDN = preFQDN
		default:
			continue
		}
		sni, _, _ := unstructured.NestedString(value, "transport_socket", "typed_config", "sni")
		endpoints, _, _ := unstructured.NestedSlice(value, "load_assignment", "endpoints")
		address := ""
		if len(endpoints) > 0 {
			endpoint, _ := endpoints[0].(map[string]any)
			lbEndpoints, _ := endpoint["lb_endpoints"].([]any)
			if len(lbEndpoints) > 0 {
				lbEndpoint, _ := lbEndpoints[0].(map[string]any)
				endpointSpec, _ := lbEndpoint["endpoint"].(map[string]any)
				addressSpec, _ := endpointSpec["address"].(map[string]any)
				socket, _ := addressSpec["socket_address"].(map[string]any)
				address, _ = socket["address"].(string)
			}
		}
		if address != wantFQDN || sni != wantFQDN {
			t.Fatalf("%s cluster address/SNI = %q/%q, want %q/%q", clusterName, address, sni, wantFQDN, wantFQDN)
		}
		if clusterName == "payload-processing-extproc" {
			foundTenantCluster = true
		} else {
			foundGatewayCluster = true
		}
	}
	if !foundTenantCluster {
		t.Fatal("EnvoyFilter post-auth cluster must target the tenant-qualified Service FQDN")
	}
	if !foundGatewayCluster {
		t.Fatal("EnvoyFilter pre-auth cluster must remain Gateway-local")
	}
	for _, resource := range resources {
		if resource.GetKind() == "ClusterRoleBinding" && resource.GetName() == PayloadProcessingReaderClusterRoleBindingPostNameForTenant("tenant-a") {
			t.Fatal("post-auth ExtProc must not bind the shared MaaS reader ClusterRole")
		}
	}
}

func TestSplitPostAuthResourcesSeparatesSameNamespaceServiceAccount(t *testing.T) {
	requireManifests(t)
	rendered, err := render.Build(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	resources := render.PostRender(rendered, render.Params{Namespace: "gateway-system", GatewayName: "gateway", Image: "extproc:dev"})
	resources, err = Rename(resources, "tenant-a", "gateway-system")
	if err != nil {
		t.Fatal(err)
	}
	resources, err = SplitPostAuthResources(resources, "tenant-a", "gateway-system", "gateway-system")
	if err != nil {
		t.Fatal(err)
	}
	post := false
	external := false
	externalService := false
	externalConfig := false
	for _, resource := range resources {
		if resource.GetKind() == "ServiceAccount" && resource.GetName() == PayloadProcessingPostServiceAccountName("tenant-a") {
			post = true
		}
		if resource.GetKind() == "ServiceAccount" && resource.GetName() == PayloadProcessingExternalModelServiceAccountName("tenant-a") {
			external = true
		}
		if resource.GetKind() == "Service" && resource.GetName() == PayloadProcessingExternalModelServiceName("tenant-a") && resource.GetNamespace() == "gateway-system" {
			externalService = true
		}
		if resource.GetKind() == "ConfigMap" && resource.GetName() == PayloadProcessingExternalModelPluginsConfigMapForTenant("tenant-a") && resource.GetNamespace() == "gateway-system" {
			externalConfig = true
		}
		if resource.GetKind() == "ClusterRoleBinding" && resource.GetName() == PayloadProcessingReaderClusterRoleBindingPostNameForTenant("tenant-a") {
			t.Fatal("same-namespace post-auth ExtProc must not bind the shared MaaS reader ClusterRole")
		}
	}
	if !post {
		t.Fatalf("missing same-namespace post-auth ServiceAccount %s", PayloadProcessingPostServiceAccountName("tenant-a"))
	}
	if !external {
		t.Fatalf("missing same-namespace ExternalModel ServiceAccount %s", PayloadProcessingExternalModelServiceAccountName("tenant-a"))
	}
	if !externalService || !externalConfig {
		t.Fatalf("missing same-namespace ExternalModel Service or ConfigMap: service=%t config=%t", externalService, externalConfig)
	}
}

#!/usr/bin/env python3
"""Qualify ExternalModel static provider weights through the Kind Gateway."""

import base64
from concurrent.futures import ThreadPoolExecutor
import datetime
import hashlib
import json
import math
import os
import pathlib
import re
import secrets
import socket
import subprocess
import time
import urllib.error
import urllib.request
import uuid
from urllib.request import HTTPRedirectHandler

ROOT = pathlib.Path(__file__).resolve().parents[2]
CLUSTER = os.environ.get("LOCAL_ENV_CLUSTER", "")
CONTEXT = f"kind-{CLUSTER}" if CLUSTER else ""
NAMESPACE = "models-as-a-service"
API_NAMESPACE = "maas-system"
API_CA_CONFIG = "authorino-maas-api-ca"
MODEL = "demo-model"
DEPLOYMENT = "payload-processing-external-model"
CONFIG_MAP = "payload-processing-external-model-plugins"
PROVIDERS = ("proof-a", "proof-b", "proof-c")
PROVIDER_RESOURCES = ("provider-proof-a", "provider-proof-b", "provider-proof-c")
SUFFIXES = ("a", "b", "c")
SAMPLE_CONCURRENCY = 8
SAMPLE_PLAN = {
    "equal-active-active-50-50": 500,
    "unequal-active-active-50-25-25": 500,
    "active-passive-100-0": 50,
    "administrative-switch-0-100": 50,
    "canary-95-5": 500,
}
STATISTICAL_ALPHA = 0.001
CHI_SQUARE_CRITICAL = {1: 10.828, 2: 13.816}
RECORDER_IMAGE = os.environ.get("PROVIDER_RECORDER_IMAGE", "provider-recorder:issue28-kind")
CONTROLLER_IMAGE = os.environ.get("AI_CONTROLLER_IMAGE", "ai-gateway-controller:external-model-two-plane")
EXTPROC_IMAGE = os.environ.get("EXTPROC_IMAGE", "praxis-extproc:dev")
NOW = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
EVIDENCE = pathlib.Path(os.environ.get("LOCAL_ENV_WEIGHT_EVIDENCE", ROOT / "evidence" / f"static-provider-weights-{NOW}"))
RESULT = {"status": "RUNNING", "assertions": [], "samples": [], "source": {},
          "statistical_plan": {"alpha_per_distribution": STATISTICAL_ALPHA,
                               "measured_distribution_windows": 3,
                               "family_wise_alpha_upper_bound": 0.003,
                               "pearson_critical_values": CHI_SQUARE_CRITICAL,
                               "sample_sizes": SAMPLE_PLAN,
                               "bounded_concurrency": SAMPLE_CONCURRENCY}}
PROCESSES = []
MODEL_BEFORE = None
MODEL_CHANGED = False
MODEL_WAS_MUTATED = False
API_KEY_ID = None
AUTHORIZATION = None
API_CA_PATH = None
ADMIN_CURL_CONFIG = None
RECOMPUTE_BINARY = None
LOCAL_PORTS = {}
API_KEY_CREATE_ATTEMPTED = False
PROVIDER_CA_PATH = None


def run(args, *, input_text=None, timeout=90, check=True):
    proc = subprocess.run(args, input=input_text, text=True, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, timeout=timeout)
    if check and proc.returncode:
        # Callers never put credentials in argv. Keep diagnostics bounded and
        # omit stdout because MaaS key creation returns the key in its body.
        detail = proc.stderr[-1000:].strip()
        raise RuntimeError(f"command failed rc={proc.returncode}: {args[0]} ({detail})")
    return proc


def kubectl(*args, timeout=90, check=True, input_text=None):
    if not CONTEXT:
        raise RuntimeError("LOCAL_ENV_CLUSTER must name the isolated Kind cluster")
    return run(["kubectl", "--context", CONTEXT, *args], timeout=timeout,
               check=check, input_text=input_text)


def write_json(path, value):
    path = pathlib.Path(path)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
    os.replace(temporary, path)


def record(name, passed, details):
    item = {"name": name, "result": "PASS" if passed else "FAIL", "details": details}
    RESULT["assertions"].append(item)
    write_json(EVIDENCE / "results.json", RESULT)
    print(f"{item['result']} {name}: {json.dumps(details, sort_keys=True)}")
    if not passed:
        raise RuntimeError(f"assertion failed: {name}")


def get_json(*args, timeout=90, check=True):
    proc = kubectl(*args, "-o", "json", timeout=timeout, check=check)
    return json.loads(proc.stdout) if proc.returncode == 0 else None


def get_json_or_not_found(*args, timeout=30):
    proc = kubectl(*args, "-o", "json", timeout=timeout, check=False)
    if proc.returncode == 0:
        return json.loads(proc.stdout)
    if "NotFound" in proc.stderr:
        return None
    raise RuntimeError(f"resource lookup failed without a NotFound response: {args[0]}")


def digest_worktree(repo):
    head = run(["git", "-C", str(repo), "rev-parse", "HEAD"]).stdout.strip()
    branch = run(["git", "-C", str(repo), "branch", "--show-current"]).stdout.strip()
    tracked = run(["git", "-C", str(repo), "diff", "--binary", "HEAD"]).stdout
    untracked = run(["git", "-C", str(repo), "ls-files", "--others", "--exclude-standard"]).stdout.splitlines()
    digest = hashlib.sha256()
    digest.update(tracked.encode())
    for relative in sorted(untracked):
        path = pathlib.Path(repo) / relative
        if path.is_file():
            digest.update(relative.encode())
            digest.update(b"\0")
            digest.update(path.read_bytes())
    status = run(["git", "-C", str(repo), "status", "--porcelain=v1", "--untracked-files=all"]).stdout
    return {"repository": str(repo), "head": head, "branch": branch,
            "dirty": bool(status), "dirty_content_sha256": digest.hexdigest(),
            "status_paths": [line[3:] for line in status.splitlines()]}


def capture_provenance():
    source = {"aigc": digest_worktree(ROOT), "controller_image": CONTROLLER_IMAGE,
              "extproc_image": EXTPROC_IMAGE, "provider_recorder_image": RECORDER_IMAGE}
    workspace = pathlib.Path(os.environ.get("LOCAL_ENV_DEPS_DIR", ROOT.parent / "deps"))
    for component, variable, fallback in (
            ("maas", "MAAS_CONTROLLER_REPO", workspace / "models-as-a-service"),
            ("extproc", "PRAXIS_EXTPROC_REPO", workspace / "praxis-extproc"),
            ("praxis_ai", "PRAXIS_AI_REPO", workspace / "praxis-ai")):
        repo = pathlib.Path(os.environ.get(variable, fallback))
        source[component] = digest_worktree(repo) if (repo / ".git").exists() else {
            "repository": str(repo), "available": False}
    images = {}
    for name, tag in (("controller", CONTROLLER_IMAGE), ("extproc", EXTPROC_IMAGE),
                      ("provider_recorder", RECORDER_IMAGE)):
        inspected = run(["docker", "image", "inspect", tag], check=False)
        if inspected.returncode == 0:
            data = json.loads(inspected.stdout)[0]
            images[name] = {"tag": tag, "id": data.get("Id"), "repo_digests": data.get("RepoDigests")}
        else:
            images[name] = {"tag": tag, "available": False}
    RESULT["source"] = source
    RESULT["local_images"] = images
    write_json(EVIDENCE / "provenance.json", {"source": source, "local_images": images})


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def start_port_forward(namespace, service, remote_port, log_name):
    local = free_port()
    log = open(EVIDENCE / log_name, "w")
    process = subprocess.Popen(["kubectl", "--context", CONTEXT, "-n", namespace,
                                "port-forward", "--address", "127.0.0.1", f"svc/{service}",
                                f"{local}:{remote_port}"], stdout=log, stderr=subprocess.STDOUT)
    PROCESSES.append((process, log))
    for _ in range(60):
        if process.poll() is not None:
            raise RuntimeError(f"port-forward exited for {namespace}/{service}; see {log_name}")
        try:
            with socket.create_connection(("127.0.0.1", local), timeout=0.25):
                LOCAL_PORTS[service] = local
                return local
        except OSError:
            time.sleep(0.5)
    raise RuntimeError(f"bounded port-forward readiness timed out for {namespace}/{service}")


def create_api_key():
    global API_KEY_ID, AUTHORIZATION, API_CA_PATH, ADMIN_CURL_CONFIG, API_KEY_CREATE_ATTEMPTED
    config = get_json("-n", "kuadrant-system", "get", "configmap", API_CA_CONFIG)
    ca = config.get("data", {}).get("ca.crt", "") if config else ""
    if not ca.strip():
        raise RuntimeError("MaaS API CA ConfigMap has no ca.crt")
    API_CA_PATH = EVIDENCE / ".maas-api-ca.crt"
    API_CA_PATH.write_text(ca)
    API_CA_PATH.chmod(0o600)
    ADMIN_CURL_CONFIG = EVIDENCE / ".admin-curl.cfg"
    ADMIN_CURL_CONFIG.write_text('header = "X-MaaS-Username: kind-user"\nheader = "X-MaaS-Group: [\\"system:authenticated\\"]"\n')
    port = start_port_forward(API_NAMESPACE, "maas-api", 8443, "maas-api-port-forward.log")
    start_port_forward(API_NAMESPACE, "maas-default-gateway", 80, "gateway-port-forward.log")
    command = ["curl", "--noproxy", "*", "--cacert", str(API_CA_PATH), "--http1.1",
               "--connect-timeout", "5", "--max-time", "25", "--resolve",
               f"maas-api.maas-system.svc.cluster.local:{port}:127.0.0.1", "-sS", "-X", "POST",
               "-o", "-", "--config", str(ADMIN_CURL_CONFIG), "-H", "content-type: application/json",
               "--data", '{"name":"static-provider-weights","ephemeral":true,"subscription":"kind-e2e-subscription"}',
               "-w", "\n%{http_code}",
               f"https://maas-api.maas-system.svc.cluster.local:{port}/v1/api-keys"]
    API_KEY_CREATE_ATTEMPTED = True
    proc = run(command, timeout=35, check=False)
    if proc.returncode:
        raise RuntimeError(f"ephemeral MaaS key creation transport failed (curl rc={proc.returncode})")
    try:
        body, status = proc.stdout.rsplit("\n", 1)
        data = json.loads(body)
        API_KEY_ID = data.get("id") or data.get("keyId") or data.get("metadata", {}).get("id")
        key = data.get("key") or data.get("apiKey") or data.get("token")
    except (ValueError, json.JSONDecodeError, AttributeError):
        raise RuntimeError("MaaS API did not return a parseable ephemeral-key response") from None
    if status != "201" or not API_KEY_ID or not key:
        raise RuntimeError(f"MaaS ephemeral-key creation returned HTTP {status} without expected fields")
    AUTHORIZATION = "Bearer " + key
    del key, data, body
    record("temporary_maas_key_created", True, {"http_status": int(status), "key_value_recorded": False})


def create_secret(namespace, name, value):
    document = {"apiVersion": "v1", "kind": "Secret",
                "metadata": {"name": name, "namespace": namespace,
                             "labels": {"app.kubernetes.io/managed-by": "external-model-e2e",
                                         "external-model-e2e/purpose": "static-provider-weights"}},
                "type": "Opaque", "data": {"api-key": base64.b64encode(value.encode()).decode()}}
    proc = kubectl("create", "-f", "-", input_text=json.dumps(document), check=False)
    if proc.returncode:
        raise RuntimeError(f"could not create run-owned credential fixture {namespace}/{name}")


def prepare_providers():
    for suffix, provider, provider_resource in zip(SUFFIXES, PROVIDERS, PROVIDER_RESOURCES):
        for kind, name, namespace in (("deployment", f"provider-selection-proof-{suffix}", API_NAMESPACE),
                                      ("externalprovider", provider_resource, NAMESPACE)):
            existing = kubectl("-n", namespace, "get", kind, name, check=False)
            if existing.returncode == 0:
                raise RuntimeError(f"refusing to reuse existing proof fixture {kind} {namespace}/{name}; use a fresh Kind cluster")
        credential = secrets.token_urlsafe(36)
        create_secret(API_NAMESPACE, f"{provider_resource}-backend-credential", credential)
        create_secret(NAMESPACE, f"{provider_resource}-credentials", credential)
        del credential
    provider_yaml = (ROOT / "test/kind-env/manifests/provider-proof-external-providers.yaml").read_text()
    deployment_yaml = (ROOT / "test/kind-env/manifests/provider-selection-proof.yaml").read_text()
    deployment_yaml = deployment_yaml.replace("provider-recorder:issue28-kind", RECORDER_IMAGE)
    kubectl("apply", "-f", "-", input_text=deployment_yaml)
    for suffix in SUFFIXES:
        kubectl("-n", API_NAMESPACE, "rollout", "status", f"deployment/provider-selection-proof-{suffix}",
                "--timeout=150s", timeout=160)
    kubectl("apply", "-f", "-", input_text=provider_yaml)
    wait_provider_backends()
    record("three_credential_enforcing_provider_fixtures_ready", True,
           {"external_providers": list(PROVIDER_RESOURCES), "backend_attribution": list(PROVIDERS),
            "tls": "verified by each attributable Gateway response and backend receipt",
            "credential_values_recorded": False})


def wait_provider_backends(deadline_seconds=240):
    deadline = time.monotonic() + deadline_seconds
    while time.monotonic() < deadline:
        ready = True
        for suffix, provider_resource in zip(SUFFIXES, PROVIDER_RESOURCES):
            dep = get_json("-n", API_NAMESPACE, "get", "deployment", f"provider-selection-proof-{suffix}", check=False)
            if not dep or dep.get("status", {}).get("readyReplicas") != 1:
                ready = False
                break
            slices = get_json("-n", API_NAMESPACE, "get", "endpointslices", "-l",
                              f"kubernetes.io/service-name={provider_resource}-ext", check=False)
            if not slices or not any(endpoint.get("conditions", {}).get("ready") is True
                                     for item in slices.get("items", [])
                                     for endpoint in item.get("endpoints", [])):
                ready = False
                break
        if ready:
            return
        time.sleep(2)
    raise RuntimeError("bounded readiness wait failed for a credential-enforcing backend or EndpointSlice")


def wait_provider_objects(deadline_seconds=240):
    """Wait until AIGC has observed every referenced provider after model reconcile."""
    deadline = time.monotonic() + deadline_seconds
    last = {}
    while time.monotonic() < deadline:
        ready = True
        for provider in PROVIDER_RESOURCES:
            provider_obj = get_json("-n", NAMESPACE, "get", "externalprovider", provider, check=False)
            if not provider_obj:
                ready = False
                break
            last[provider] = provider_obj.get("status", {})
            status = last[provider]
            if (status.get("phase") != "Ready"
                    or status.get("observedGeneration") != provider_obj.get("metadata", {}).get("generation")
                    or not any(condition.get("type") == "Ready" and condition.get("status") == "True"
                               for condition in status.get("conditions", []))):
                ready = False
                break
        if ready:
            return
        time.sleep(2)
    write_json(EVIDENCE / "provider-readiness-timeout.json", last)
    raise RuntimeError("bounded readiness wait failed for referenced ExternalProvider conditions")


def make_ref(provider, weight):
    return {"ref": {"name": provider}, "targetModel": "demo", "apiFormat": "openai-chat",
            "path": "/v1/chat/completions", "weight": weight}


def set_weights(weights):
    global MODEL_CHANGED, MODEL_WAS_MUTATED
    refs = [make_ref(provider, weights[suffix]) for suffix, provider in zip(SUFFIXES, PROVIDER_RESOURCES)]
    kubectl("-n", NAMESPACE, "patch", "externalmodel", MODEL, "--type=merge", "-p",
            json.dumps({"spec": {"externalProviderRefs": refs}}))
    MODEL_CHANGED = True
    MODEL_WAS_MUTATED = True
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        current = get_json("-n", NAMESPACE, "get", "externalmodel", MODEL, check=False)
        if current:
            metadata = current.get("metadata", {})
            status = current.get("status", {})
            observed = status.get("observedGeneration") == metadata.get("generation")
            if observed and status.get("phase") == "Ready":
                return current
            if observed and status.get("phase") == "Failed":
                failure = {
                    "name": metadata.get("name"),
                    "namespace": metadata.get("namespace"),
                    "generation": metadata.get("generation"),
                    "observed_generation": status.get("observedGeneration"),
                    "phase": status.get("phase"),
                    "conditions": [{key: condition.get(key) for key in ("type", "status", "reason")}
                                   for condition in status.get("conditions", [])],
                }
                write_json(EVIDENCE / "model-generation-failed.json", failure)
                raise RuntimeError("ExternalModel weight update reconciled Failed at its observed generation; see model-generation-failed.json")
        time.sleep(1)
    raise RuntimeError("ExternalModel did not become Ready at its changed generation")


def ratio_signature(weights):
    active = [value for value in weights.values() if value > 0]
    if len(active) < 2 or len(set(active)) == 1:
        return None
    divisor = math.gcd(*active)
    return {suffix: (value // divisor if value > 0 else None) for suffix, value in weights.items()}


def all_zero_withdrawal_ready(model, route, overlay):
    if model is None or (overlay or {}).get("invalid"):
        return False
    metadata = (model or {}).get("metadata", {})
    status = (model or {}).get("status", {})
    no_routes = any(condition.get("type") == "Ready"
                    and condition.get("status") == "False"
                    and condition.get("reason") == "NoRoutes"
                    for condition in status.get("conditions", []))
    candidates = (overlay or {}).get("overlay", {}).get("candidates", [])
    model_candidates = [candidate for candidate in candidates
                        if candidate.get("stable_id", "").startswith(f"{MODEL}/")]
    return (metadata.get("generation") == status.get("observedGeneration")
            and status.get("phase") == "Failed" and no_routes
            and route is None and not model_candidates)


def wait_for_all_zero_withdrawal(route_name, timeout_seconds=240):
    deadline = time.monotonic() + timeout_seconds
    last = {}
    while time.monotonic() < deadline:
        model = get_json_or_not_found("-n", NAMESPACE, "get", "externalmodel", MODEL)
        route = (get_json_or_not_found("-n", NAMESPACE, "get", "httproute", route_name)
                 if route_name else None)
        config_map = get_json_or_not_found("-n", NAMESPACE, "get", "configmap", "routing-overlay")
        overlay = None
        if config_map:
            try:
                overlay = json.loads(config_map.get("data", {}).get("routing-overlay.json", "{}"))
            except json.JSONDecodeError:
                overlay = {"invalid": True}
        last = {"generation": (model or {}).get("metadata", {}).get("generation"),
                "observed_generation": (model or {}).get("status", {}).get("observedGeneration"),
                "phase": (model or {}).get("status", {}).get("phase"),
                "conditions": [{key: condition.get(key) for key in ("type", "status", "reason")}
                               for condition in (model or {}).get("status", {}).get("conditions", [])],
                "route_absent": route is None, "overlay_absent": config_map is None,
                "model_candidates_present": bool(overlay and any(
                    candidate.get("stable_id", "").startswith(f"{MODEL}/")
                    for candidate in overlay.get("overlay", {}).get("candidates", [])))}
        if all_zero_withdrawal_ready(model, route, overlay):
            write_json(EVIDENCE / "all-zero-withdrawal-convergence.json", last)
            return model, last
        time.sleep(2)
    write_json(EVIDENCE / "all-zero-withdrawal-timeout.json", last)
    raise RuntimeError("all-zero generation did not withdraw the model route and overlay candidate")


def one_request_receipt_result(response, receipts, request_id, expected_provider):
    matches = [receipt for receipt in receipts if receipt.get("request_id") == request_id]
    if expected_provider is None:
        return {"pass": response.get("request_id") == request_id
                and response.get("status") == 404 and not response.get("provider")
                and not matches, "http_status": response.get("status"),
                "provider_attribution": response.get("provider", ""),
                "provider_receipt_count": len(matches)}
    valid_receipt = (len(matches) == 1 and matches[0].get("provider") == expected_provider
                     and matches[0].get("method") == "POST"
                     and matches[0].get("path") == "/v1/chat/completions"
                     and matches[0].get("model") == "demo"
                     and all(matches[0].get(key) is True
                             for key in ("tls", "sni_ok", "authority_ok", "credential_ok")))
    return {"pass": response.get("request_id") == request_id and response.get("status") == 200
            and response.get("provider") == expected_provider
            and response.get("checks") is True and valid_receipt,
            "http_status": response.get("status"),
            "provider_attribution": response.get("provider", ""),
            "provider_receipt_count": len(matches),
            "receipt_provider": matches[0].get("provider", "") if len(matches) == 1 else "",
            "receipt_transport_and_credential_checks": bool(valid_receipt)}


def qualify_all_zero_withdrawal(initial_uid):
    global MODEL_CHANGED, MODEL_WAS_MUTATED
    model = get_json("-n", NAMESPACE, "get", "externalmodel", MODEL)
    route_name = model.get("status", {}).get("httpRouteName", "")
    if not route_name or not get_json("-n", NAMESPACE, "get", "httproute", route_name, check=False):
        raise RuntimeError("all-zero withdrawal requires the controller-published serving HTTPRoute")

    refs = [make_ref(provider, 0) for provider in PROVIDER_RESOURCES]
    kubectl("-n", NAMESPACE, "patch", "externalmodel", MODEL, "--type=merge", "-p",
            json.dumps({"spec": {"externalProviderRefs": refs}}))
    MODEL_CHANGED = True
    MODEL_WAS_MUTATED = True
    withdrawn, withdrawal_state = wait_for_all_zero_withdrawal(route_name)
    withdrawal_id = "static-weights-all-zero-withdrawal-" + uuid.uuid4().hex
    withdrawal_response = gateway_request(withdrawal_id)
    withdrawal_receipts = backend_receipts([withdrawal_id], "all-zero-withdrawal")
    withdrawal = one_request_receipt_result(withdrawal_response, withdrawal_receipts, withdrawal_id, None)
    record("all_zero_weights_withdraw_serving_route", withdrawal["pass"],
           {**withdrawal, "generation": withdrawn.get("metadata", {}).get("generation"),
            "observed_generation": withdrawn.get("status", {}).get("observedGeneration"),
            "phase": withdrawn.get("status", {}).get("phase"),
            "withdrawal_state": withdrawal_state,
            "request_id": withdrawal_id})

    set_weights({"a": 1, "b": 0, "c": 0})
    active_state = wait_for_state({"a": 1, "b": 0, "c": 0}, "all-zero-reactivation")
    request_id = "static-weights-all-zero-reactivation-" + uuid.uuid4().hex
    response = gateway_request(request_id)
    receipts = backend_receipts([request_id], "all-zero-reactivation")
    reactivation = one_request_receipt_result(response, receipts, request_id, "proof-a")
    after_request = capture_overlay_state("all-zero-reactivation-after-request")
    stable = (active_state.get("declared_digest") == after_request.get("declared_digest")
              and active_state.get("generation") == after_request.get("generation")
              and active_state.get("runtime", {}).get("pod_uid") == initial_uid
              == after_request.get("runtime", {}).get("pod_uid")
              and after_request.get("runtime", {}).get("restart_count") == 0)
    record("all_zero_reactivation_provider_a_serves", reactivation["pass"] and stable,
           {**reactivation, "serving_configuration_stable": stable,
            "extproc_uid_unchanged": bool(after_request.get("runtime", {}).get("pod_uid") == initial_uid),
            "extproc_restart_count": after_request.get("runtime", {}).get("restart_count"),
            "request_id": request_id})
    RESULT["all_zero_weights"] = {
        "withdrawal": withdrawal,
        "reactivation": {**reactivation, "serving_configuration_stable": stable},
    }


def model_ready():
    model = get_json("-n", NAMESPACE, "get", "externalmodel", MODEL, check=False)
    return bool(model and model.get("status", {}).get("phase") == "Ready"
                and model.get("status", {}).get("observedGeneration") == model.get("metadata", {}).get("generation"))


def route_ready():
    model = get_json("-n", NAMESPACE, "get", "externalmodel", MODEL, check=False)
    route_name = (model or {}).get("status", {}).get("httpRouteName")
    if not route_name:
        return False
    route = get_json("-n", NAMESPACE, "get", "httproute", route_name, check=False)
    if not route:
        return False
    for parent in route.get("status", {}).get("parents", []):
        conditions = {condition.get("type"): condition.get("status")
                      for condition in parent.get("conditions", [])}
        if conditions.get("Accepted") == "True" and conditions.get("ResolvedRefs") == "True":
            return True
    return False


def deployment_extproc_config_hash(deployment):
    annotations = (deployment.get("spec", {}).get("template", {})
                   .get("metadata", {}).get("annotations", {}))
    return annotations.get("ai-gateway-controller.opendatahub.io/extproc-config-sha256", "")


def overlay_revisions_from_logs(logs):
    logs = re.sub(r"\x1b\[[0-9;]*m", "", logs)
    for line in reversed(logs.splitlines()):
        match = re.search(r'accepted_revision\s*=\s*"([0-9a-f]{64})".*?serving_revision\s*=\s*"([0-9a-f]{64})"', line)
        if match:
            return match.groups()
    return "", ""


def get_pod_and_runtime():
    deployment = get_json("-n", NAMESPACE, "get", "deployment", DEPLOYMENT)
    pod_template = deployment.get("spec", {}).get("template", {})
    template = pod_template.get("spec", {})
    selector = deployment.get("spec", {}).get("selector", {}).get("matchLabels", {})
    selector_text = ",".join(f"{key}={value}" for key, value in sorted(selector.items()))
    pods = get_json("-n", NAMESPACE, "get", "pods", "-l", selector_text)
    active = [pod for pod in pods.get("items", []) if not pod.get("metadata", {}).get("deletionTimestamp")
              and any(condition.get("type") == "Ready" and condition.get("status") == "True"
                      for condition in pod.get("status", {}).get("conditions", []))]
    if len(active) != 1:
        return None
    pod = active[0]
    container_statuses = {item.get("name"): item for item in pod.get("status", {}).get("containerStatuses", [])}
    containers = pod.get("spec", {}).get("containers", [])
    ext_container = next((container for container in containers if EXTPROC_IMAGE == container.get("image")), None)
    if not ext_container:
        return None
    ext_status = container_statuses.get(ext_container.get("name"), {})
    if not ext_status.get("ready"):
        return None
    config_obj = get_json("-n", NAMESPACE, "get", "configmap", CONFIG_MAP)
    config_text = config_obj.get("data", {}).get("extproc.yaml", "")
    mounted_config = kubectl("-n", NAMESPACE, "exec", f"pod/{pod['metadata']['name']}", "-c",
                             ext_container["name"], "--", "cat", "/etc/praxis/extproc.yaml", check=False, timeout=20)
    if mounted_config.returncode:
        return None
    cluster_line = next((line.strip() for line in config_text.splitlines() if "provider_hop_clusters:" in line), "")
    cluster_match = re.search(r"provider_hop_clusters:\s*(\[.*\])", cluster_line)
    clusters = json.loads(cluster_match.group(1)) if cluster_match else []
    credential_entries = []
    pattern = re.compile(r"(?m)^\s+- name: ([^\n]+)\n\s+namespace: ([^\n]+)\n\s+key: ([^\n]+)\n\s+strategy: ([^\n]+)\n\s+file: ([^\n]+)$")
    for match in pattern.finditer(config_text):
        credential_entries.append({"name": match.group(1).strip(), "namespace": match.group(2).strip(),
                                   "key": match.group(3).strip(), "strategy": match.group(4).strip(),
                                   "file": match.group(5).strip()})
    projected = []
    for volume in template.get("volumes", []):
        if volume.get("name") != "provider-credentials":
            continue
        for source in volume.get("projected", {}).get("sources", []):
            secret = source.get("secret", {})
            projected.extend({"name": secret.get("name"), "key": item.get("key"), "path": item.get("path")}
                             for item in secret.get("items", []))
    ext_hash = hashlib.sha256(config_text.encode()).hexdigest()
    logs = kubectl("-n", NAMESPACE, "logs", f"pod/{pod['metadata']['name']}", "-c",
                   ext_container["name"], "--tail=300", check=False).stdout
    accepted, serving = overlay_revisions_from_logs(logs)
    return {"deployment_generation": deployment.get("metadata", {}).get("generation"),
            "deployment_observed_generation": deployment.get("status", {}).get("observedGeneration"),
            "deployment_replicas": deployment.get("status", {}).get("replicas"),
            "deployment_ready_replicas": deployment.get("status", {}).get("readyReplicas"),
            "deployment_template_sha256": deployment_extproc_config_hash(deployment),
            "extproc_config_sha256": ext_hash,
            "mounted_extproc_sha256": hashlib.sha256(mounted_config.stdout.encode()).hexdigest(),
            "runtime_clusters": sorted(clusters), "credential_entries": credential_entries,
            "projected_secrets": sorted(projected, key=lambda item: item["name"] or ""),
            "pod_uid": pod.get("metadata", {}).get("uid"),
            "restart_count": ext_status.get("restartCount", 0),
            "image": ext_container.get("image"), "image_id": ext_status.get("imageID"),
            "accepted_revision": accepted, "serving_revision": serving}


def capture_overlay_state(label):
    cm = get_json("-n", NAMESPACE, "get", "configmap", "routing-overlay")
    raw = cm.get("data", {}).get("routing-overlay.json", "")
    wire = json.loads(raw)
    path = EVIDENCE / f".{label}-published-overlay.json"
    path.write_text(raw)
    recomputed = run([str(RECOMPUTE_BINARY), str(path)]).stdout.strip()
    mounted_result = kubectl("-n", NAMESPACE, "exec", f"deploy/{DEPLOYMENT}", "--", "cat",
                             "/etc/praxis/routing/routing-overlay.json", check=False, timeout=20)
    mounted_digest = ""
    mounted_wire = {}
    mounted_candidates = []
    if mounted_result.returncode == 0:
        mounted_path = EVIDENCE / f".{label}-mounted-overlay.json"
        mounted_path.write_text(mounted_result.stdout)
        mounted_digest = run([str(RECOMPUTE_BINARY), str(mounted_path)]).stdout.strip()
        mounted_wire = json.loads(mounted_result.stdout)
        mounted_candidates = [{key: candidate.get(key) for key in (
            "cluster", "stable_id", "kind", "name", "selection_group", "traffic_weight")}
            for candidate in mounted_wire.get("overlay", {}).get("candidates", [])]
        mounted_path.unlink(missing_ok=True)
    path.unlink(missing_ok=True)
    overlay = wire.get("overlay", {})
    candidates = []
    for candidate in overlay.get("candidates", []):
        candidates.append({key: candidate.get(key) for key in (
            "cluster", "stable_id", "kind", "name", "selection_group", "traffic_weight")})
    revision = wire.get("revision", {})
    annotations = cm.get("metadata", {}).get("annotations", {})
    runtime = get_pod_and_runtime()
    state = {"label": label, "declared_digest": annotations.get(
                 "inference.opendatahub.io/routing-overlay-content-digest", ""),
             "recomputed_digest": recomputed, "mounted_digest": mounted_digest,
             "generation": int(annotations.get("inference.opendatahub.io/routing-overlay-source-generation", "0")),
             "wire_generation": int(wire.get("provenance", {}).get("source_generation", 0)),
             "wire_revision": revision.get("value", ""),
             "selection_policy": overlay.get("selection_policy"), "candidates": candidates,
             "mounted_candidates": mounted_candidates,
             "runtime": runtime, "model_ready": model_ready(), "route_ready": route_ready()}
    return state


def expected_wire_ok(state, weights):
    active = {suffix: weight for suffix, weight in weights.items() if weight > 0}
    cluster_to_suffix = {f"provider-{provider}": suffix
                         for suffix, provider in zip(SUFFIXES, PROVIDER_RESOURCES)}
    actual = {candidate.get("cluster"): candidate for candidate in state.get("candidates", [])}
    if set(actual) != {f"provider-{PROVIDER_RESOURCES[SUFFIXES.index(suffix)]}" for suffix in active}:
        return False
    expected_policy = None
    active_values = [weight for weight in active.values()]
    if len(active_values) > 1:
        expected_policy = {"mode": "weightedRandom" if len(set(active_values)) > 1 else "random"}
    if state.get("selection_policy") != expected_policy:
        return False
    for cluster, candidate in actual.items():
        suffix = cluster_to_suffix.get(cluster)
        if suffix is None or candidate.get("selection_group") != (0 if len(active) > 1 else None):
            return False
        if len(active) <= 1 or len(set(active.values())) == 1:
            if candidate.get("traffic_weight") is not None:
                return False
        elif candidate.get("traffic_weight") != active[suffix]:
            return False
    return True


def state_is_ready(state, weights):
    runtime = state.get("runtime") or {}
    expected_all = sorted(f"provider-{provider}" for provider in PROVIDER_RESOURCES)
    # The projected path is generated by the controller; validate Secret and key
    # identities without recording any Secret contents.
    projected_names = {item.get("name") for item in runtime.get("projected_secrets", [])}
    credential_names = {item.get("name") for item in runtime.get("credential_entries", [])}
    return (state.get("model_ready") and state.get("route_ready")
            and state.get("declared_digest") and state["declared_digest"] == state.get("recomputed_digest")
            == state.get("mounted_digest") == state.get("wire_revision")
            and state.get("generation") == state.get("wire_generation") > 0
            and state.get("mounted_candidates") == state.get("candidates")
            and expected_wire_ok(state, weights)
            and runtime.get("runtime_clusters") == expected_all
            and credential_names == {f"{provider}-credentials" for provider in PROVIDER_RESOURCES}
            and projected_names == {f"{provider}-credentials" for provider in PROVIDER_RESOURCES}
            and runtime.get("configmap_extproc_sha256", runtime.get("extproc_config_sha256")) == runtime.get("extproc_config_sha256")
            and runtime.get("mounted_extproc_sha256") == runtime.get("extproc_config_sha256")
            and runtime.get("deployment_template_sha256") == runtime.get("extproc_config_sha256")
            and runtime.get("deployment_observed_generation") == runtime.get("deployment_generation")
            and runtime.get("deployment_replicas") == runtime.get("deployment_ready_replicas") == 1
            and runtime.get("restart_count") == 0)


def baseline_state_is_ready(state, expected_clusters):
    runtime = state.get("runtime") or {}
    model_candidates = [candidate for candidate in state.get("candidates", [])
                        if candidate.get("stable_id", "").startswith(f"{MODEL}/")]
    active_clusters = sorted(candidate.get("cluster") for candidate in model_candidates)
    digest_values = (state.get("declared_digest"), state.get("recomputed_digest"),
                    state.get("mounted_digest"), state.get("wire_revision"),
                    runtime.get("accepted_revision"), runtime.get("serving_revision"))
    return (state.get("model_ready") and state.get("route_ready")
            and all(digest_values) and len(set(digest_values)) == 1
            and state.get("generation") == state.get("wire_generation") > 0
            and active_clusters == sorted(expected_clusters)
            and runtime.get("deployment_observed_generation") == runtime.get("deployment_generation")
            and runtime.get("deployment_replicas") == runtime.get("deployment_ready_replicas") == 1
            and runtime.get("deployment_template_sha256") == runtime.get("extproc_config_sha256")
            and runtime.get("mounted_extproc_sha256") == runtime.get("extproc_config_sha256")
            and runtime.get("restart_count") == 0)


def wait_for_provider_a_baseline(timeout_seconds=240):
    expected_clusters = sorted(
        f"provider-{ref.get('ref', {}).get('name')}"
        for ref in MODEL_BEFORE.get("spec", {}).get("externalProviderRefs", [])
        if ref.get("weight", 1) > 0)
    deadline = time.monotonic() + timeout_seconds
    previous = None
    stable = 0
    last = {}
    while time.monotonic() < deadline:
        last = capture_overlay_state("provider-a-reset")
        fingerprint = json.dumps(last, sort_keys=True)
        if baseline_state_is_ready(last, expected_clusters) and fingerprint == previous:
            stable += 1
            if stable >= 2:
                write_json(EVIDENCE / "provider-a-reset-convergence.json", last)
                return True
        else:
            stable = 0
        previous = fingerprint if baseline_state_is_ready(last, expected_clusters) else None
        time.sleep(2)
    write_json(EVIDENCE / "provider-a-reset-convergence-last.json", last)
    return False


def wait_for_state(weights, label, timeout_seconds=480):
    deadline = time.monotonic() + timeout_seconds
    previous = None
    stable = 0
    last = {}
    while time.monotonic() < deadline:
        last = capture_overlay_state(label)
        ready = state_is_ready(last, weights)
        fingerprint = json.dumps({key: last.get(key) for key in (
            "declared_digest", "recomputed_digest", "mounted_digest", "generation",
            "wire_generation", "wire_revision", "selection_policy", "candidates", "runtime",
            "model_ready", "route_ready")}, sort_keys=True)
        if ready and fingerprint == previous:
            stable += 1
            if stable >= 2:
                write_json(EVIDENCE / f"convergence-{label}.json", last)
                return last
        else:
            stable = 0
        previous = fingerprint if ready else None
        time.sleep(2)
    write_json(EVIDENCE / f"convergence-{label}-last.json", last)
    raise RuntimeError(f"overlay/provider/route serving revision did not converge twice: {label}")


def affinity_disabled():
    runtime = get_pod_and_runtime()
    if not runtime:
        return False
    config = kubectl("-n", NAMESPACE, "get", "configmap", CONFIG_MAP,
                     "-o", "jsonpath={.data.extproc.yaml}").stdout
    enabled = re.search(r"(?ms)^\s*session_affinity:\s*\{[^}]*enabled:\s*true", config)
    return enabled is None


class RejectRedirects(HTTPRedirectHandler):
    def redirect_request(self, _request, _response, _code, _message, _headers, _new_url):
        return None


def gateway_request(request_id):
    payload = b'{"model":"demo","messages":[{"role":"user","content":"static-weight qualification"}]}'
    headers = {"Authorization": AUTHORIZATION, "Content-Type": "application/json",
               "X-Request-ID": request_id, "X-Issue28-Request-ID": request_id}
    request = urllib.request.Request(
        f"http://127.0.0.1:{LOCAL_PORTS['maas-default-gateway']}/{NAMESPACE}/demo/v1/chat/completions",
        data=payload, headers=headers, method="POST")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), RejectRedirects())
    try:
        with opener.open(request, timeout=20) as response:
            status, body = response.status, response.read()
    except urllib.error.HTTPError as error:
        status, body = error.code, error.read()
    except Exception as error:  # transport failures are recorded once and never retried
        return {"request_id": request_id, "status": 0, "provider": "", "checks": False,
                "transport_error": type(error).__name__}
    try:
        result = json.loads(body)
    except (json.JSONDecodeError, UnicodeDecodeError):
        result = {}
    return {"request_id": request_id, "status": status,
            "provider": response_provider(result),
            "checks": isinstance(result, dict) and all(
                result.get(key) is True for key in ("tls", "sni_ok", "authority_ok", "credential_ok"))}


def response_provider(payload):
    """Extract only fixture attribution; never return or persist response text."""
    if not isinstance(payload, dict):
        return ""
    direct = payload.get("provider") or payload.get("backend")
    if direct:
        return str(direct)
    choices = payload.get("choices")
    if not isinstance(choices, list) or not choices or not isinstance(choices[0], dict):
        return ""
    message = choices[0].get("message")
    content = message.get("content", "") if isinstance(message, dict) else ""
    if isinstance(content, str):
        if "katan-a-" in content:
            return "provider-a"
        if "katan-b-" in content:
            return "provider-b"
    return ""


def sample_gateway_requests(request_ids):
    """Send every independent sample request once with bounded concurrency."""
    with ThreadPoolExecutor(max_workers=SAMPLE_CONCURRENCY) as executor:
        return list(executor.map(gateway_request, request_ids))


def parse_observation_receipts(body, wanted):
    receipts = []
    for line in body.splitlines():
        try:
            item = json.loads(line)
        except json.JSONDecodeError:
            continue
        if item.get("request_id") not in wanted:
            continue
        receipts.append({key: item.get(key) for key in (
            "request_id", "provider", "method", "path", "authority", "tls", "sni_ok",
            "authority_ok", "model", "credential_ok", "x_api_key_present", "authorization_present")})
    return receipts


def provider_observation_ca():
    global PROVIDER_CA_PATH
    if PROVIDER_CA_PATH:
        return PROVIDER_CA_PATH
    config = get_json("-n", API_NAMESPACE, "get", "configmap", "external-model-e2e-provider-ca", check=False)
    ca = (config or {}).get("data", {}).get("ca.crt", "")
    if not ca.strip():
        raise RuntimeError("run-owned provider CA ConfigMap has no ca.crt")
    PROVIDER_CA_PATH = EVIDENCE / ".provider-observations-ca.crt"
    PROVIDER_CA_PATH.write_text(ca)
    PROVIDER_CA_PATH.chmod(0o600)
    return PROVIDER_CA_PATH


def fetch_provider_observations(suffix):
    provider_resource = PROVIDER_RESOURCES[SUFFIXES.index(suffix)]
    service = provider_resource + "-ext"
    port = LOCAL_PORTS.get(service)
    if not port:
        port = start_port_forward(API_NAMESPACE, service, 443, f"receipt-port-forward-{suffix}.log")
    host = service + ".maas-system.svc.cluster.local"
    command = ["curl", "--noproxy", "*", "--cacert", str(provider_observation_ca()),
               "--resolve", f"{host}:{port}:127.0.0.1", "--connect-timeout", "5", "--max-time", "30",
               "--silent", "--show-error", "--output", "-", "--write-out", "\n%{http_code}",
               f"https://{host}:{port}/__test/observations"]
    response = run(command, timeout=35, check=False)
    if response.returncode:
        raise RuntimeError(f"provider {suffix} observation endpoint transport failed")
    try:
        body, status = response.stdout.rsplit("\n", 1)
    except ValueError:
        raise RuntimeError(f"provider {suffix} observation endpoint returned an invalid response") from None
    if status != "200":
        raise RuntimeError(f"provider {suffix} observation endpoint returned HTTP {status}")
    return body


def backend_receipts(request_ids, label):
    wanted = set(request_ids)
    receipts = []
    for suffix in SUFFIXES:
        receipts.extend(parse_observation_receipts(fetch_provider_observations(suffix), wanted))
    write_json(EVIDENCE / f"backend-receipts-{label}.json", receipts)
    return receipts


def receipt_storage_smoke():
    """Prove one request is persisted and retrievable before large samples."""
    request_id = "static-weights-receipt-smoke-" + uuid.uuid4().hex
    response = gateway_request(request_id)
    receipts = backend_receipts([request_id], "receipt-storage-smoke")
    matches = [receipt for receipt in receipts if receipt.get("request_id") == request_id]
    valid = (response.get("status") == 200 and response.get("provider") == "proof-a"
             and response.get("checks") and len(matches) == 1
             and matches[0].get("provider") == "proof-a"
             and matches[0].get("method") == "POST"
             and matches[0].get("path") == "/v1/chat/completions"
             and matches[0].get("model") == "demo"
             and all(matches[0].get(key) is True
                     for key in ("tls", "sni_ok", "authority_ok", "credential_ok")))
    record("recorder_receipt_storage_smoke", valid,
           {"http_status": response.get("status"), "response_provider": response.get("provider"),
            "receipt_count": len(matches), "receipt_provider": matches[0].get("provider") if matches else "",
            "tls_sni_authority_credentials_verified": bool(matches) and all(
                matches[0].get(key) is True for key in ("tls", "sni_ok", "authority_ok", "credential_ok")),
            "request_id": request_id})


def sample_window(name, weights, requests, chi_square_df=None):
    before = wait_for_state(weights, f"{name}-before")
    if not affinity_disabled():
        record("session_affinity_disabled", False, {"extproc_config": "session affinity enabled"})
    ids = ["static-weights-" + uuid.uuid4().hex for _ in range(requests)]
    responses = sample_gateway_requests(ids)
    counts = {provider: 0 for provider in PROVIDERS}
    status_counts = {}
    with (EVIDENCE / f"sample-{name}.jsonl").open("w") as stream:
        for response in responses:
            status_counts[str(response["status"])] = status_counts.get(str(response["status"]), 0) + 1
            if response["provider"] in counts:
                counts[response["provider"]] += 1
            stream.write(json.dumps(response, sort_keys=True) + "\n")
            stream.flush()
    receipts = backend_receipts(ids, name)
    by_id = {}
    for receipt in receipts:
        by_id.setdefault(receipt.get("request_id"), []).append(receipt)
    receipt_errors = []
    response_by_id = {response["request_id"]: response for response in responses}
    for request_id in ids:
        response = response_by_id[request_id]
        matches = by_id.get(request_id, [])
        if len(matches) != 1:
            receipt_errors.append({"request_id": request_id, "receipt_count": len(matches)})
            continue
        receipt = matches[0]
        if (receipt.get("provider") != response.get("provider")
                or not receipt.get("tls") or not receipt.get("sni_ok")
                or not receipt.get("authority_ok") or not receipt.get("credential_ok")
                or receipt.get("method") != "POST"
                or receipt.get("path") != "/v1/chat/completions"
                or receipt.get("model") != "demo"):
            receipt_errors.append({"request_id": request_id, "receipt_provider": receipt.get("provider"),
                                   "response_provider": response.get("provider"),
                                   "transport_and_credential_checks": False})
    successful = all(response.get("status") == 200 and response.get("checks")
                     and response.get("provider") in PROVIDERS for response in responses)
    n = len(responses)
    failed = n != requests or not successful
    expected = {provider: weights[suffix] for suffix, provider in zip(SUFFIXES, PROVIDERS) if weights[suffix] > 0}
    chi_square = None
    statistic_ok = True
    if chi_square_df is not None:
        active_counts = {provider: counts[provider] for provider in expected}
        total_expected_weight = sum(expected.values())
        chi_square = sum((active_counts[provider] - n * weight / total_expected_weight) ** 2
                         / (n * weight / total_expected_weight) for provider, weight in expected.items())
        critical = CHI_SQUARE_CRITICAL.get(chi_square_df)
        statistic_ok = critical is not None and chi_square <= critical
    disabled_receipts = {provider: sum(item.get("provider") == provider for item in receipts)
                         for suffix, provider in zip(SUFFIXES, PROVIDERS) if weights[suffix] == 0}
    all_receipts = (len(receipts) == len(ids) and not receipt_errors)
    result = {"name": name, "requested": requests, "completed": n, "provider_counts": counts,
              "http_status_counts": status_counts, "failed_or_unattributed": sum(
                  response.get("status") != 200 or not response.get("checks")
                  or response.get("provider") not in PROVIDERS for response in responses),
              "expected_positive_providers": sorted(expected), "disabled_provider_receipts": disabled_receipts,
              "receipt_count": len(receipts), "receipt_errors": receipt_errors,
              "all_requests_have_one_attributed_receipt": all_receipts,
              "chi_square": chi_square, "chi_square_df": chi_square_df,
              "chi_square_alpha": STATISTICAL_ALPHA if chi_square_df is not None else None,
              "chi_square_critical": CHI_SQUARE_CRITICAL.get(chi_square_df),
              "configuration_before": {key: before.get(key) for key in (
                  "declared_digest", "generation", "selection_policy", "candidates", "runtime")},
              "request_ids": ids}
    after = capture_overlay_state(f"{name}-after")
    result["configuration_after"] = {key: after.get(key) for key in (
        "declared_digest", "generation", "selection_policy", "candidates", "runtime")}
    stable_keys = ("declared_digest", "recomputed_digest", "mounted_digest", "generation", "wire_generation",
                   "selection_policy", "candidates")
    result["serving_configuration_stable"] = all(before.get(key) == after.get(key) for key in stable_keys)
    result["extproc_identity_stable"] = bool(before.get("runtime") and after.get("runtime")
        and before["runtime"].get("pod_uid") == after["runtime"].get("pod_uid")
        and before["runtime"].get("restart_count") == after["runtime"].get("restart_count") == 0
        and before["runtime"].get("deployment_generation") == after["runtime"].get("deployment_generation")
        and before["runtime"].get("extproc_config_sha256") == after["runtime"].get("extproc_config_sha256"))
    write_json(EVIDENCE / f"sample-{name}.json", result)
    RESULT["samples"].append({key: value for key, value in result.items() if key != "request_ids"})
    write_json(EVIDENCE / "results.json", RESULT)
    active_and_disabled = all(counts[p] == 0 for p in disabled_receipts) if disabled_receipts else True
    passed = (not failed and all_receipts and result["serving_configuration_stable"]
              and result["extproc_identity_stable"] and active_and_disabled and statistic_ok
              and all(count > 0 for provider, count in counts.items() if provider in expected))
    record(name, passed, {key: value for key, value in result.items() if key != "request_ids"})
    return result, after


def set_and_converge(weights, label):
    generation = set_weights(weights)
    if label == "weighted-50-25-25":
        wait_provider_objects()
    state = wait_for_state(weights, label)
    return generation, state


def baseline_katan_request():
    request_id = "static-weights-katan-" + uuid.uuid4().hex
    response = gateway_request(request_id)
    baseline_refs = MODEL_BEFORE.get("spec", {}).get("externalProviderRefs", [])
    expected_provider = baseline_refs[0].get("ref", {}).get("name", "") if baseline_refs else ""
    record("llm_d_katan_provider_a_baseline",
           response["status"] == 200 and response.get("provider") == expected_provider,
           {"http_status": response["status"], "request_id": request_id,
            "provider": response.get("provider"), "expected_provider": expected_provider,
            "backend": "existing llm-d-katan Provider A fixture", "body_recorded": False})


def clean_owned_fixture_objects():
    # Delete only exact resources created by this test after ownership checks.
    for suffix, provider, provider_resource in zip(SUFFIXES, PROVIDERS, PROVIDER_RESOURCES):
        obj = get_json("-n", NAMESPACE, "get", "externalprovider", provider_resource, check=False)
        if obj and obj.get("metadata", {}).get("labels", {}).get("external-model-e2e/purpose") == "provider-selection-proof":
            kubectl("-n", NAMESPACE, "delete", "externalprovider", provider_resource, "--wait=true", "--timeout=120s", timeout=130)
        for kind, name, namespace in (("secret", f"{provider_resource}-credentials", NAMESPACE),
                                      ("secret", f"{provider_resource}-backend-credential", API_NAMESPACE),
                                      ("deployment", f"provider-selection-proof-{suffix}", API_NAMESPACE),
                                      ("service", f"{provider_resource}-ext", API_NAMESPACE)):
            current = get_json("-n", namespace, "get", kind, name, check=False)
            if not current:
                continue
            if current.get("metadata", {}).get("labels", {}).get("app.kubernetes.io/managed-by") != "external-model-e2e":
                raise RuntimeError(f"refusing cleanup of unowned {kind} {namespace}/{name}")
            kubectl("-n", namespace, "delete", kind, name, "--wait=true", "--timeout=120s", timeout=130)


def restore_model():
    global MODEL_CHANGED
    if not MODEL_CHANGED or not MODEL_BEFORE:
        return
    original_refs = MODEL_BEFORE.get("spec", {}).get("externalProviderRefs", [])
    kubectl("-n", NAMESPACE, "patch", "externalmodel", MODEL, "--type=merge", "-p",
            json.dumps({"spec": {"externalProviderRefs": original_refs}}))
    deadline = time.monotonic() + 240
    while time.monotonic() < deadline:
        current = get_json("-n", NAMESPACE, "get", "externalmodel", MODEL, check=False)
        if current and current.get("status", {}).get("phase") == "Ready" and current.get("status", {}).get("observedGeneration") == current.get("metadata", {}).get("generation"):
            MODEL_CHANGED = False
            return
        time.sleep(2)
    raise RuntimeError("Provider A baseline model did not recover at its current generation")


def verify_provider_a_reset():
    if not MODEL_WAS_MUTATED or MODEL_CHANGED:
        return
    if not wait_for_provider_a_baseline():
        raise RuntimeError("Provider A overlay and route did not converge before the reset probe")
    request_id = "static-weights-provider-a-reset-" + uuid.uuid4().hex
    response = gateway_request(request_id)
    baseline_refs = MODEL_BEFORE.get("spec", {}).get("externalProviderRefs", [])
    expected_provider = baseline_refs[0].get("ref", {}).get("name", "") if baseline_refs else ""
    record("provider_a_baseline_reset",
           response["status"] == 200 and response.get("provider") == expected_provider,
           {"http_status": response["status"], "request_id": request_id,
            "provider": response.get("provider"), "expected_provider": expected_provider,
            "backend": "existing llm-d-katan Provider A", "body_recorded": False})


def revoke_key():
    global API_KEY_ID, AUTHORIZATION
    if not API_KEY_ID:
        return None
    port = LOCAL_PORTS.get("maas-api")
    if not port:
        return None
    proc = run(["curl", "--noproxy", "*", "--cacert", str(API_CA_PATH), "--http1.1",
                "--connect-timeout", "5", "--max-time", "20", "--resolve",
                f"maas-api.maas-system.svc.cluster.local:{port}:127.0.0.1", "-sS", "-o", "/dev/null",
                "-w", "%{http_code}", "--config", str(ADMIN_CURL_CONFIG), "-X", "DELETE",
                f"https://maas-api.maas-system.svc.cluster.local:{port}/v1/api-keys/{API_KEY_ID}"],
               timeout=25, check=False)
    status = proc.stdout.strip() if proc.returncode == 0 else "transport-error"
    API_KEY_ID = None
    AUTHORIZATION = None
    return status


def shutdown_port_forwards():
    for process, log in PROCESSES:
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        log.close()


def main():
    global MODEL_BEFORE, RECOMPUTE_BINARY
    if not CLUSTER:
        raise RuntimeError("set LOCAL_ENV_CLUSTER to a fresh, uniquely named Kind cluster")
    if EVIDENCE.exists() and any(EVIDENCE.iterdir()):
        raise RuntimeError(f"evidence directory already contains files; use a fresh path: {EVIDENCE}")
    EVIDENCE.mkdir(parents=True, exist_ok=True)
    ctx = run(["kubectl", "config", "get-contexts", CONTEXT, "-o", "name"], check=False)
    if ctx.returncode or ctx.stdout.strip() != CONTEXT:
        raise RuntimeError(f"explicit Kind context is unavailable: {CONTEXT}")
    if not get_json("get", "namespace", NAMESPACE, check=False):
        raise RuntimeError(f"isolated Kind cluster is not provisioned: {CONTEXT}")
    capture_provenance()
    RECOMPUTE_BINARY = EVIDENCE / "recompute-overlay-digest"
    run(["go", "build", "-o", str(RECOMPUTE_BINARY), str(ROOT / "test/kind-env/recompute_digest.go")], timeout=180)
    MODEL_BEFORE = get_json("-n", NAMESPACE, "get", "externalmodel", MODEL)
    if not MODEL_BEFORE:
        raise RuntimeError("expected demo-model baseline ExternalModel is missing")
    write_json(EVIDENCE / "model-baseline-reference-identities.json",
               {"namespace": NAMESPACE, "name": MODEL,
                "model_name": MODEL_BEFORE.get("spec", {}).get("modelName"),
                "provider_refs": [{"name": ref.get("ref", {}).get("name"), "namespace": ref.get("ref", {}).get("namespace", ""),
                                   "weight": ref.get("weight", 1)}
                                  for ref in MODEL_BEFORE.get("spec", {}).get("externalProviderRefs", [])]})
    create_api_key()
    baseline_katan_request()
    prepare_providers()
    set_and_converge({"a": 1, "b": 0, "c": 0}, "receipt-storage-smoke")
    receipt_storage_smoke()

    # All three provider objects and credential projections are present before
    # the first weighted serving revision. Subsequent operations change weights
    # only, so the serving ExtProc workload must not roll.
    refs = {"a": 50, "b": 25, "c": 25}
    _, initial = set_and_converge(refs, "weighted-50-25-25")
    initial_uid = initial["runtime"]["pod_uid"]
    initial_generation = initial["runtime"]["deployment_generation"]
    record("provider_preload_and_initial_weighted_revision", True,
           {"active_weights": refs, "runtime_clusters": initial["runtime"]["runtime_clusters"],
            "credential_projection_names": sorted(item["name"] for item in initial["runtime"]["projected_secrets"]),
            "extproc_pod_uid": initial_uid, "deployment_generation": initial_generation})

    # Equal shares retain the previously supported random policy.
    _, equal = set_and_converge({"a": 50, "b": 50, "c": 0}, "equal-50-50")
    sample_window("equal-active-active-50-50", {"a": 50, "b": 50, "c": 0},
                  SAMPLE_PLAN["equal-active-active-50-50"], chi_square_df=1)

    # Pearson goodness-of-fit critical values were fixed before observing counts:
    # alpha=0.001, df=2 => 13.816; df=1 => 10.828.
    _, first_ratio = set_and_converge(refs, "unequal-50-25-25")
    unequal_result, ratio_after = sample_window("unequal-active-active-50-25-25", refs,
                                                SAMPLE_PLAN["unequal-active-active-50-25-25"], chi_square_df=2)
    if not unequal_result["serving_configuration_stable"]:
        raise RuntimeError("50:25:25 serving configuration changed during sampling")

    # Weights are passed through as configured. Ratios are equivalent for
    # selection, but the distinct configured wire weights are distinct
    # content-addressed revisions.
    previous_digest = first_ratio["declared_digest"]
    previous_generation = first_ratio["generation"]
    _, equivalent = set_and_converge({"a": 2, "b": 1, "c": 1}, "equivalent-2-1-1")
    record("relative_ratio_equivalence", ratio_signature(refs) == ratio_signature({"a": 2, "b": 1, "c": 1})
           and equivalent["declared_digest"] != previous_digest
           and equivalent["generation"] == previous_generation + 1
           and equivalent["runtime"]["pod_uid"] == initial_uid
           and equivalent["runtime"]["deployment_generation"] == initial_generation,
           {"50-25-25_ratio_signature": ratio_signature(refs),
            "2-1-1_ratio_signature": ratio_signature({"a": 2, "b": 1, "c": 1}),
            "50-25-25_digest": previous_digest, "2-1-1_digest": equivalent["declared_digest"],
            "generation_advanced_for_wire_change": equivalent["generation"] == previous_generation + 1,
            "extproc_uid_unchanged": equivalent["runtime"]["pod_uid"] == initial_uid})
    # The converged overlay above proves 2:1:1 is rendered as configured and
    # has the same ratio signature as 50:25:25. No second traffic sample is
    # needed to establish ratio equivalence.

    # Active/passive is an explicit administrative configuration update, not failover.
    _, only_a = set_and_converge({"a": 100, "b": 0, "c": 0}, "active-passive-100-0")
    sample_window("active-passive-100-0", {"a": 100, "b": 0, "c": 0},
                  SAMPLE_PLAN["active-passive-100-0"])
    _, only_b = set_and_converge({"a": 0, "b": 100, "c": 0}, "active-passive-0-100")
    sample_window("administrative-switch-0-100", {"a": 0, "b": 100, "c": 0},
                  SAMPLE_PLAN["administrative-switch-0-100"])

    _, canary = set_and_converge({"a": 95, "b": 5, "c": 0}, "canary-95-5")
    sample_window("canary-95-5", {"a": 95, "b": 5, "c": 0},
                  SAMPLE_PLAN["canary-95-5"], chi_square_df=1)
    qualify_all_zero_withdrawal(initial_uid)

    if any(state["runtime"]["pod_uid"] != initial_uid for state in (equal, first_ratio, ratio_after, equivalent, only_a, only_b, canary)):
        raise RuntimeError("weight-only update changed the ExternalModel ExtProc pod UID")


def cleanup():
    errors = []
    try:
        restore_model()
        verify_provider_a_reset()
    except Exception as error:
        errors.append(f"provider-a-reset: {type(error).__name__}: {error}")
    status = revoke_key()
    if status is not None:
        try:
            record("temporary_maas_key_revoked", status in ("200", "204"),
                   {"http_status": int(status) if status.isdigit() else status,
                    "key_value_recorded": False})
        except Exception as error:
            errors.append(f"key-revocation: {type(error).__name__}: {error}")
    elif API_KEY_CREATE_ATTEMPTED:
        RESULT["cleanup_failure"] = True
        RESULT.setdefault("cleanup", []).append("API-key creation was attempted but revocation could not be proven")
    if (all(item.get("result") == "PASS" for item in RESULT.get("assertions", []))
            and "first_failure" not in RESULT and not errors):
        try:
            clean_owned_fixture_objects()
            RESULT["cleanup"] = ["Provider A baseline restored", "ephemeral MaaS key revoked",
                                 "run-owned proof ExternalProviders, Secrets, Deployments, and Services removed"]
        except Exception as error:
            errors.append(f"fixture-cleanup: {type(error).__name__}: {error}")
    else:
        RESULT["cleanup"] = ["failed-run fixture resources preserved for diagnosis"]
    if MODEL_CHANGED:
        errors.append("model reference restoration did not complete")
    if errors:
        RESULT["cleanup_failure"] = errors
    shutdown_port_forwards()
    for path in (API_CA_PATH, PROVIDER_CA_PATH, ADMIN_CURL_CONFIG):
        if path and pathlib.Path(path).exists():
            pathlib.Path(path).unlink()
    RESULT["status"] = "PASS" if (RESULT.get("assertions")
        and all(item.get("result") == "PASS" for item in RESULT["assertions"])
        and "first_failure" not in RESULT
        and not RESULT.get("cleanup_failure") and not MODEL_CHANGED) else "FAIL"
    write_json(EVIDENCE / "results.json", RESULT)
    patterns = [
        re.compile(rb"(?i)authorization\s*[:=]\s*(?:bearer|basic)\s+[^\s\"']{12,}"),
        re.compile(rb"(?i)bearer\s+[a-z0-9._~+/-]{24,}"),
        re.compile(rb"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----"),
        re.compile(rb"\bsk-[A-Za-z0-9_-]{20,}\b"),
    ]
    findings = []
    for path in EVIDENCE.rglob("*"):
        if not path.is_file():
            continue
        try:
            content = path.read_bytes()
        except OSError:
            findings.append(path.relative_to(EVIDENCE).as_posix())
            continue
        if any(pattern.search(content) for pattern in patterns):
            findings.append(path.relative_to(EVIDENCE).as_posix())
    write_json(EVIDENCE / "credential-scan.json", {
        "scan_scope": "all files under this qualification evidence directory",
        "patterns_checked": ["Authorization bearer/basic values", "bearer token shapes",
                              "private-key PEM markers", "OpenAI-style secret key prefix"],
        "finding_files": findings,
        "status": "FAIL" if findings else "PASS"})
    if findings:
        RESULT["credential_scan_failure"] = findings
        RESULT["status"] = "FAIL"
        write_json(EVIDENCE / "results.json", RESULT)


if __name__ == "__main__":
    if EVIDENCE.exists() and any(EVIDENCE.iterdir()):
        raise SystemExit(f"evidence directory already contains files: {EVIDENCE}")
    EVIDENCE.mkdir(parents=True, exist_ok=True)
    exit_code = 0
    try:
        main()
    except Exception as error:
        RESULT["status"] = "FAIL"
        RESULT["first_failure"] = {"type": type(error).__name__, "message": str(error)}
        write_json(EVIDENCE / "failure.json", RESULT["first_failure"])
        print(f"FAIL static provider weights: {type(error).__name__}: {error}")
        exit_code = 1
    finally:
        try:
            cleanup()
        except Exception as error:
            RESULT["cleanup_failure"] = f"{type(error).__name__}: {error}"
            RESULT["status"] = "FAIL"
            write_json(EVIDENCE / "results.json", RESULT)
            exit_code = 1
    if RESULT.get("status") != "PASS":
        exit_code = 1
    raise SystemExit(exit_code)

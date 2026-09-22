# Praxis dataplane evolution

This document describes how the MaaS AI Gateway request path evolves from
today's IPP + Kuadrant stack toward Praxis as the full AI gateway.
It is an evolution map for the **dataplane request path**, not a
deployment-topology ADR — see [DESIGN.md](../DESIGN.md) for operator /
controller ownership.

Source notes:
[Praxis evolution (gist)](https://gist.github.com/aslakknutsen/ff7f9a7fc206b0706974a7756054e56d).

## Stages at a glance

| Stage | Envoy role | Auth / rate limit | AI filters | Endpoint pick |
| --- | --- | --- | --- | --- |
| Current (IPP) | Only L7 dataplane | Kuadrant Wasm → Authorino / Limitador | `ipp-pre` + `ipp` ext_proc | InferencePool EPP ext_proc |
| Praxis for 3.6 | Only L7 dataplane | Kuadrant Wasm (unchanged) | `praxis-pre` + `praxis` ext_proc | InferencePool EPP ext_proc |
| Post 3.6 | Only L7 dataplane | Moved into Praxis pipeline | Single `praxis` ext_proc | InferencePool EPP ext_proc |
| Praxis + EPP | Only L7 dataplane | In Praxis | In Praxis | In Praxis (no EPP hop) |
| Praxis complete | Router / LB only | In Praxis | In Praxis | In Praxis; Praxis proxies upstream |

Throughout every Envoy-based stage there is still **one** Gateway Envoy
instance on the path. Extra hops are gRPC ext_proc (or Wasm) side calls from
that Envoy, not additional Envoy proxies.

---

## Current architecture (IPP)

```mermaid
flowchart TB
  Client["Client<br/>POST /v1/chat/completions<br/>Authorization: Bearer &lt;maas-api-key&gt;"]
  Router["OpenShift Router<br/>(optional; ClusterIP+Route)"]
  Envoy["Envoy — maas-default-gateway<br/>only L7 dataplane Envoy"]

  subgraph filters ["Envoy HTTP filter chain"]
    direction TB
    IPPPre["1. ext_proc ipp-pre<br/>payload-pre-processing:9004"]
    Kuadrant["2. Kuadrant Wasm<br/>(not ext_proc)"]
    IPP["3. ext_proc ipp<br/>payload-processing:9004"]
    Route["4. HTTPRoute match<br/>backendRef: InferencePool"]
    EPP["5. ext_proc EPP / scheduler<br/>pick InferencePool pod"]
  end

  Authorino["Authorino<br/>→ maas-api validate / subscription"]
  Limitador["Limitador<br/>(optional rate limit)"]
  Upstream["vLLM pod<br/>/v1/chat/completions"]

  Client --> Router --> Envoy
  Envoy --> IPPPre
  IPPPre -->|"model → X-Gateway-Model-Name"| Kuadrant
  Kuadrant --> Authorino
  Kuadrant --> Limitador
  Kuadrant -->|"allow/deny; inject x-maas-*; strip client Auth"| IPP
  IPP -->|"strip x-maas-*; rewrite publishers/... model"| Route
  Route --> EPP
  EPP -->|"selected endpoint IP:port"| Upstream
  Upstream -->|"response"| Envoy
  Envoy -->|"ipp may see response; ipp-pre does not;<br/>EPP is not a second response hop"| Client
```

**What each hop does (LLMISvc):**

1. **ipp-pre** — read body `model`, set `X-Gateway-Model-Name` (mostly header
   extract for on-cluster LLMISvc).
2. **Kuadrant Wasm** — auth via Authorino (may call maas-api for API key,
   subscription, model access); optional Limitador; inject `x-maas-*`
   identity headers; strip/replace client `Authorization`.
3. **ipp** — strip `x-maas-*` and client auth leftovers; rewrite
   `publishers/...` body model → bare name; no provider API-key injection
   for on-cluster.
4. **HTTPRoute** — match path / publisher headers; backend is an
   **InferencePool**, not a normal Service.
5. **EPP** — pick best pod in the pool (load / KV-cache / queue aware).
6. **Proxy** — Envoy forwards HTTP to the chosen pod.

---

## Praxis for 3.6

Same Envoy filter ordering as today; IPP binaries become Praxis profiles.

```mermaid
flowchart TB
  Client["Client<br/>Bearer &lt;maas-api-key&gt;"]
  Router["OpenShift Router<br/>(optional)"]
  Envoy["Envoy — maas-default-gateway<br/>only L7 dataplane Envoy"]

  subgraph filters ["Envoy HTTP filter chain"]
    direction TB
    PraxisPre["1. ext_proc praxis-pre<br/>request only — light pipeline"]
    Kuadrant["2. Kuadrant Wasm"]
    Praxis["3. ext_proc praxis<br/>request + response — full AI pipeline"]
    Route["4. HTTPRoute match"]
    EPP["5. ext_proc EPP<br/>(InferencePool only)"]
  end

  Authorino["Authorino<br/>POST maas-api /api-keys/validate<br/>POST maas-api /subscriptions/select"]
  Limitador["Limitador<br/>(TokenRateLimitPolicy)"]
  Upstream["vLLM or external provider"]

  Client --> Router --> Envoy
  Envoy --> PraxisPre
  PraxisPre -->|"classify + promote model<br/>→ X-Gateway-Model-Name"| Kuadrant
  Kuadrant --> Authorino
  Kuadrant --> Limitador
  Kuadrant -->|"inject x-maas-*; strip client Auth"| Praxis
  Praxis -->|"LLMISvc: rewrite model if needed<br/>ExternalModel: inject provider creds + rewrite"| Route
  Route -->|"LLMISvc → InferencePool<br/>External → Service egress (skip EPP)"| EPP
  EPP --> Upstream
  Upstream --> Envoy
  Envoy -->|"praxis response phase; praxis-pre: no response"| Client
```

**3.6 delta vs current:** filter roles stay the same; `ipp-pre` / `ipp` are
replaced by `praxis-pre` / `praxis`. Kuadrant and EPP remain side calls from
Envoy. ExternalModel routes skip EPP and backend to an egress Service.

---

## Praxis + Kuadrant post 3.6

Auth and rate limiting move into the Praxis filter pipeline. Kuadrant Wasm
drops out of the hot path. Envoy still owns routing + EPP.

```mermaid
flowchart TB
  Client["Client"]
  Router["OpenShift Router<br/>(optional)"]
  Envoy["Envoy — maas-default-gateway"]

  subgraph praxis ["1. ext_proc praxis — request + response"]
    direction TB
    A["a. classify / model → X-Gateway-Model-Name"]
    B["b. auth filter"]
    C["c. rate-limit filter"]
    D["d. AI filters<br/>enrich / guardrails / translate / strip creds / inject provider key / token headers"]
    E["e. set routing headers"]
    A --> B --> C --> D --> E
  end

  MaasAPI["maas-api<br/>validate + subscription select<br/>(or TokenReview / OIDC)"]
  Limitador["Limitador<br/>(optional; or in-process quota)"]
  Route["2. HTTPRoute match"]
  EPP["3. ext_proc EPP<br/>(InferencePool only)"]
  Upstream["vLLM or external provider"]

  Client --> Router --> Envoy
  Envoy --> praxis
  B --> MaasAPI
  C --> Limitador
  praxis -->|"mutated request or immediate deny"| Route
  Route -->|"LLMISvc → InferencePool<br/>External → Service (skip EPP)"| EPP
  EPP --> Upstream
  Upstream --> Envoy
  Envoy -->|"same praxis stream: usage / translate / …"| Client
```

---

## Praxis + Kuadrant + EPP

Endpoint selection moves into Praxis. InferencePool → EPP ext_proc is gone;
Envoy sends traffic to the upstream Praxis selected.

```mermaid
flowchart TB
  Client["Client"]
  Router["OpenShift Router<br/>(optional)"]
  Envoy["Envoy — maas-default-gateway"]

  subgraph praxis ["1. ext_proc praxis — request + response"]
    direction TB
    A["a. classify / model header"]
    B["b. auth → maas-api"]
    C["c. rate-limit → Limitador (optional)"]
    D["d. AI filters"]
    E["e. endpoint pick (ex-EPP)<br/>watch pool state / metrics<br/>choose vLLM pod<br/>inject routing signal for Envoy"]
    A --> B --> C --> D --> E
  end

  Route["2. HTTPRoute / upstream selection<br/>no InferencePool→EPP ext_proc<br/>Envoy uses Praxis-chosen endpoint"]
  Upstream["vLLM pod or external provider FQDN"]

  Client --> Router --> Envoy
  Envoy --> praxis
  praxis -->|"mutated request + selected upstream<br/>or deny from auth/rl"| Route
  Route --> Upstream
  Upstream --> Envoy
  Envoy -->|"praxis response: usage / translate / …"| Client
```

---

## Praxis complete

Praxis becomes the gateway (e.g. Pingora + AI filters). Envoy is no longer
on the AI request path; the OpenShift Router / LB only terminates or
passes through TLS.

```mermaid
flowchart TB
  Client["Client<br/>Bearer &lt;maas-api-key&gt;"]
  LB["OpenShift Router / LB<br/>TLS terminate or pass-through only"]

  subgraph praxis ["Praxis — the Gateway"]
    direction TB
    C1["1. classify / route facts<br/>model → internal headers / cluster inputs"]
    C2["2. auth (ex-Kuadrant / Authorino)<br/>→ maas-api validate + subscription<br/>(or TokenReview / OIDC)"]
    C3["3. rate-limit (ex-Limitador)<br/>→ Limitador or in-process counters"]
    C4["4. AI payload filters (ex-IPP)<br/>enrich / guardrails / translate / creds / tokens"]
    C5["5. endpoint pick (ex-EPP)<br/>choose vLLM pod or external target"]
    C6["6. PROXY — Praxis dials upstream"]
    C1 --> C2 --> C3 --> C4 --> C5 --> C6
  end

  Upstream["vLLM pod or external provider"]

  Client --> LB --> praxis
  C2 -.->|"deny → response to client, stop"| Client
  C3 -.->|"deny → stop"| Client
  C6 --> Upstream
  Upstream -->|"response stream/body<br/>translate / usage / metering"| praxis
  praxis --> Client
```

---

## How this relates to this repository

| Concern | Owner today / near-term | Evolution target |
| --- | --- | --- |
| ExtProc install + ExternalModel control plane | `ai-gateway-controller` (this repo) | Unchanged through Envoy stages; dataplane binary becomes richer |
| Auth / subscription / API keys | Kuadrant → Authorino → maas-api | Fold into Praxis pipeline post 3.6 |
| Rate limits | Limitador via Kuadrant | Optional Limitador or in-process in Praxis |
| AI payload processing | IPP → Praxis ext_proc (3.6) | Same filters, fewer Envoy hops over time |
| InferencePool scheduling | EPP ext_proc | Absorbed by Praxis, then Praxis-as-gateway |

For control-plane deployment topology (operators, tenant fan-out, IPP vs
Praxis selection), see [DESIGN.md](../DESIGN.md).

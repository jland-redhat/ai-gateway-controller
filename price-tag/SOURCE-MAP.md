# Source Map

This repository is the public home of the PriceTag metering service.

## Source

- Source repository: historical `noyitz/ai-gateway-metering-service`, migrated
  into this repository without squashing its service history.
- Current source baseline: the `redhat-et/pricetag-metering` snapshot commit
  `8a6eadb`, followed by the history-preservation merge and dashboard freshness
  restoration.
- History migration: the legacy service commits remain reachable from `main`,
  preserving original authors, dates, and merge commits.
- Cleanup baseline: the merged Headroom removal cleanup.
- New module path: `github.com/redhat-et/pricetag-metering`

## Deliberately Excluded

- Headroom integration and compression dashboard material.
- Legacy IPP/Envoy/Istio dogfood runbooks.
- Credentials, cluster-specific secrets, and personal environment files.
- Partner-owned deployment history.

The deployment and OpenShift composition layer lives in
[`redhat-et/pricetag`](https://github.com/redhat-et/pricetag).

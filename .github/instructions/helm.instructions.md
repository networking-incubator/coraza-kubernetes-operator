---
applyTo: "charts/**"
---

- CRDs in `crds/` must be identical to `config/crd/bases/`. Run `make helm.sync` after any API type change and verify the copied files match.
- RBAC rules in the ClusterRole template must stay in sync with `config/rbac/role.yaml`. Run `make helm.sync` after any change to `kubebuilder:rbac` markers.
- All configurable values must appear in `values.yaml` with sensible defaults. Helpers that reference `.Values.*` keys not present in `values.yaml` should be avoided.
- Istio-specific resources (ServiceEntry, DestinationRule) must be gated behind `{{ if .Values.istio.enabled }}`.
- PDB defaults must be safe for the default `replicaCount`. A PDB with `minAvailable >= replicaCount` blocks voluntary evictions.

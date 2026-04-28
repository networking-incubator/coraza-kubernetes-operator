---
applyTo: "api/**/*.go"
---

- Any change to types in this directory affects the CRD schema. Verify that `make manifests` and `make generate` have been run and the results committed.
- Check for CEL validation markers (kubebuilder comments). New fields should have appropriate validation.
- Enum fields must use `+kubebuilder:validation:Enum=` markers.
- Default values must use `+default=` markers. When a field has `+default`, use `+optional` (not `+required`) and `omitempty` in the JSON tag — the API server fills in the default on create, so the field is never truly absent after admission.
- Required fields must be marked as required using kubebuilder markers; Go doc comments should focus on describing field semantics rather than restating "required".
- Immutable fields must use a CEL transition rule with `oldSelf`: `+kubebuilder:validation:XValidation:rule="self == oldSelf",message="fieldName is immutable; create a new Resource to change it"`. Place this marker on the field itself (not the parent struct). This is the standard Kubernetes pattern — no webhooks needed.
- Cross-field validation (e.g., "provider X only supports driver type Y") belongs as a CEL rule on the parent struct using `+kubebuilder:validation:XValidation`. When referencing optional fields in CEL, guard with `has()` or check for empty string to handle the zero-value case from `omitempty`/`omitzero`.
- The `zz_generated.deepcopy.go` file must be regenerated when types change. It should never be hand-edited.

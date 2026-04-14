---
title: "Known Limitations"
linkTitle: "Known Limitations"
weight: 40
description: "Behavioral differences and limitations when running Coraza WAF with Istio in WASM mode."
---

This page describes known limitations when running the Coraza WAF with Istio using WASM mode. These limitations are specific to the WASM execution environment and do not apply to all deployment modes.

## Overview

Out of approximately 3,300 OWASP CoreRuleSet conformance tests, 190 tests (6%) are currently excluded, resulting in a **94% pass rate**. The excluded tests fall into four categories:

| Category | Tests | Impact |
|----------|-------|--------|
| Enhanced Security | 45 | Positive -- Envoy provides additional protection. |
| Tool Limitations | 113 | Requires alternative controls or monitoring. |
| Coraza/WASM Bugs | 13 | Requires fixes in Coraza or coraza-proxy-wasm. |
| Under Investigation | 19 | Requires further analysis. |

## Operator Behavior

The RuleSet controller automatically detects and rejects any RuleSet containing rules listed in this document. When unsupported rules are found:

- The RuleSet is marked **Degraded** with reason `UnsupportedRules`.
- The rejected revision is not cached. Any previously cached valid revision continues to be served.
- The RuleSet is not requeued automatically. You must remove or replace the unsupported rules.

This behavior can be overridden with the annotation `waf.k8s.coraza.io/skip-unsupported-rules-check: "true"`. The issues are still logged, but the RuleSet is cached normally.

Unsupported rules are classified into two tiers:

| Tier | Meaning |
|------|---------|
| **Incompatible** | Rules that do not function in WASM mode. They fail silently and provide no protection. |
| **Redundant** | Rules that work, but where Envoy handles certain attack cases before they reach the WAF. |

Both tiers are rejected by default.

## Enhanced Security (45 tests)

These tests fail because Envoy blocks or sanitizes attacks before they reach the WAF. This represents defense-in-depth, not a security gap.

### Invalid HTTP Methods and Malformed Requests (9 tests)

Envoy rejects invalid HTTP methods and malformed requests with HTTP 400 before reaching the WASM filter.

**Affected rules:** 911100, 920100, 949110

### HTTP/2 Protocol Normalization (11 tests)

Envoy normalizes missing or empty HTTP headers during HTTP/2 processing to enforce protocol compliance.

**Affected rules:** 920280, 920290, 920300, 920310, 920311, 920320, 920330, 920610

Examples of normalization:
- Missing Host header is crafted from the `:authority` pseudo-header.
- Missing Accept header gets a default value.
- Empty Content-Type on POST is normalized.

### Malicious Header Sanitization (20 tests)

Envoy sanitizes HTTP headers containing malicious payloads (XSS, SQLi, RCE) before they reach the WAF.

**Affected rules:** 932161, 932207, 932237, 932239, 941101, 941110, 941120, 942280

### Additional Protocol Enforcement (5 tests)

Envoy enforces HTTP protocol specifications, such as rejecting Content-Length with Transfer-Encoding (prevents smuggling) and stripping the Connection header.

**Affected rules:** 920100, 920181, 920210

## Tool Limitations (113 tests)

These represent actual limitations that may allow certain attacks to bypass detection. Alternative controls are recommended.

### Response Body Inspection (79 tests)

Response body content is not available for inspection by phase 4 rules in the WASM environment. Envoy does not pass response body content to WASM filters.

**Affected rule families:** 950xxx, 951xxx, 952xxx, 953xxx, 954xxx, 955xxx, 956xxx

**Impact:** Data leakage, error messages, and web shells in responses are not detected.

**Mitigation:**
- Implement application-level logging and monitoring.
- Use SIEM integration for data leakage detection.
- Monitor response status codes via Envoy access logs.

### Multipart Charset Detection (30 tests)

Illegal charset detection in multipart form headers does not function in the Envoy/Kubernetes environment.

**Affected rule:** 922110

**Mitigation:**
- Implement application-level charset validation.
- Use Content-Type validation at the Envoy level.

### PL4 False Positives (4 tests)

Paranoia Level 4 generates false positives due to Envoy populating the `:path` pseudo-header with characters detected as invalid.

**Affected rules:** 920274

**Mitigation:** Use Paranoia Level 3 or create custom rule exceptions.

## Coraza/WASM Bugs (13 tests)

These are bugs in Coraza or coraza-proxy-wasm that should be fixed upstream.

| Issue | Tests | Affected Rules |
|-------|-------|----------------|
| GET/HEAD with body not detected | 2 | 920171 |
| Invalid HTTP protocol version not detected | 4 | 920430 |
| Enclosed alphanumerics not detected in HTTP/2 | 5 | 934120 |
| Multiphase evaluation failures | 2 | 932300, 933120 |

## Under Investigation (19 tests)

These require further analysis:

- Behavioral differences (3 tests): attacks blocked by the filter instead of Envoy.
- Cookie detection (2 tests): `$Version` parameter not matched.
- `match_regex` behavior (3 tests): different error message patterns.
- Early detection (1 test): detected by an earlier rule than expected.
- XSS edge cases (2 tests): TinyGo regex and HTML entity decoding issues.
- Java deserialization (1 test): pattern not detected.
- PL4 false positives (2 tests): benign requests trigger anomaly score.
- `retry_once` mechanism (5 tests): not working in the current environment.

## Version Information

| Component | Version |
|-----------|---------|
| Coraza | v3.7.0 |
| CoreRuleSet | v4.24.1 |
| Execution Environment | Envoy/Istio WASM filter |

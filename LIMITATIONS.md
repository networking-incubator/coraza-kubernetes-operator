# Known Limitations

This document describes behavioral differences and limitations when running Coraza WAF with Istio using the WASM mode.
	
> **Note**: We are looking at solutions for these limitations in the [Engine Modes Epic]
	
[Engine Modes Epic]:https://github.com/networking-incubator/coraza-kubernetes-operator/issues/171

## Operator Behavior

The RuleSet controller **automatically detects and rejects** any RuleSet containing rules listed in this document. When unsupported rules are found:

- The RuleSet is marked **Degraded** with reason `UnsupportedRules` and a message listing each unsupported rule ID, its category, and a brief description.
- The new, rejected revision is **not cached**. If a previous valid revision was already cached, that last-known-good entry continues to be served until the unsupported rules are removed and a new valid revision is reconciled.
- The RuleSet will not be requeued automatically — the user must remove or replace the unsupported rules and update the ConfigMap(s) to produce a new, valid cached revision.
- This behavior can be overridden with the annotation `waf.k8s.coraza.io/skip-unsupported-rules-check: "true"`. The issue will still be logged and reported, but wont block.
Unsupported rules are classified into two tiers:

| Tier | Meaning |
|------|---------|
| **Incompatible** | Rules that genuinely do not function in WASM mode. They fail silently and provide no protection. |
| **Redundant** | Rules that work, but where Envoy handles certain attack cases before they reach the WAF. Some conformance test cases behave differently. |

Both tiers are rejected by default. For the complete list of affected rule IDs, see the categories below.

## Overview

Out of approximately 3,300 CoreRuleSet conformance tests, 190 tests (6%) are currently ignored, resulting in a 94% pass rate. These ignored tests fall into four categories:

| Category | Tests | Impact |
|----------|-------|--------|
| Enhanced Security | 45 | Positive - Envoy provides additional protection |
| Tool Limitations | 113 | Requires alternative controls or monitoring |
| Coraza/WASM Bugs | 13 | Requires fixes in Coraza or coraza-proxy-wasm |
| Under Investigation | 19 | Requires further analysis |

---

## Enhanced Security (45 tests)

These tests fail because Envoy blocks or sanitizes attacks before they reach the WAF. This represents defense-in-depth, not a security gap.

### Invalid HTTP Methods and Malformed Requests (9 tests)

Envoy rejects invalid HTTP methods and malformed requests with HTTP 400 before reaching the WASM filter.

**Affected rules:** 911100, 920100, 949110

**Impact:** Positive. Malformed requests are blocked at the proxy layer, preventing protocol-level attacks.

### HTTP/2 Protocol Normalization (11 tests)

Envoy normalizes missing or empty HTTP headers during HTTP/2 processing to enforce protocol compliance.

**Affected rules:** 920280, 920290, 920300, 920310, 920311, 920320, 920330, 920610

**Examples:**
- Missing Host header: Crafted from `:authority` pseudo-header
- Missing Accept header: Default value added
- Empty Content-Type on POST: Normalized

**Impact:** Positive. Ensures protocol compliance and prevents attacks relying on missing headers.

### Malicious Header Sanitization (20 tests)

Envoy sanitizes HTTP headers containing malicious payloads (XSS, SQLi, RCE) before they reach the WAF.

**Affected rules:** 932161, 932207, 932237, 932239, 941101, 941110, 941120, 942280

**Examples:**
- Referer headers with invalid characters: Removed
- Referer headers with injection payloads: Sanitized

**Impact:** Positive. Attacks are blocked at the proxy layer before WAF inspection.

### Additional Protocol Enforcement (5 tests)

Envoy enforces HTTP protocol specifications:
- Content-Length with Transfer-Encoding: Rejected (prevents smuggling)
- Connection header: Stripped (correct hop-by-hop behavior)
- Valid HTTP methods (disabled by default in Apache/Nginx only): Accepted

**Affected rules:** 920100, 920181, 920210

**Impact:** Positive. Correct proxy behavior per HTTP specifications.

---

## Tool Limitations (113 tests)

These represent actual limitations that may allow attacks to bypass detection. Alternative controls are recommended.

### Response Body Inspection (79 tests)

Response body content is not available for inspection by phase 4 rules in the WASM environment.

**Affected rule families:**
- 950xxx: Data leakage detection (9 tests)
- 951xxx: SQL error messages (20 tests)
- 952xxx: Java error messages (10 tests)
- 953xxx: PHP error messages (9 tests)
- 954xxx: IIS error messages (7 tests)
- 955xxx: Web shell detection (10 tests)
- 956xxx: Ruby error messages (14 tests)

**Impact:** Data leakage, error messages, and web shells in responses are not detected.

**Root cause:** Envoy does not currently pass response body content to WASM filters for inspection, despite `SecResponseBodyAccess On` configuration.

**Mitigation:**
- Deploy as standalone reverse proxy for full response inspection
- Implement application-level logging and monitoring
- Use SIEM integration for data leakage detection
- Monitor response status codes via Envoy access logs

### Multipart Charset Detection (30 tests)

Illegal charset detection in multipart form headers does not function in the Envoy/Kubernetes environment.

**Affected rule:** 922110 (all 30 test cases)

**Impact:** Charset-based encoding attacks (utf-7, utf-16, shift-jis, etc.) in multipart uploads are not detected.

**Root cause:** Under investigation. The functionality works correctly in coraza-proxy-wasm unit tests but fails when deployed in Envoy/Kubernetes, suggesting an Envoy request body handling issue rather than a WASM limitation.

**Mitigation:**
- Implement application-level charset validation
- Use Content-Type validation at Envoy level
- Monitor for updates to Envoy/coraza-proxy-wasm

### PL4 False Positives (4 tests)

Paranoia Level 4 generates false positives due to Envoy populating the `:path` pseudo-header with characters detected as invalid.

**Affected rules:** 920274

**Impact:** Benign requests may be blocked at PL4.

**Mitigation:** Use Paranoia Level 3 or create custom rule exceptions.

---

## Coraza/WASM Bugs (13 tests)

These issues should be fixed in Coraza or coraza-proxy-wasm.

### GET/HEAD with Body (2 tests)

Rule 920171 does not detect GET or HEAD requests with message bodies.

**Affected rule:** 920171

**Impact:** HTTP request smuggling attacks using GET/HEAD with bodies may bypass detection.

### Invalid Protocol Version (4 tests)

Rule 920430 does not detect invalid HTTP protocol versions.

**Affected rule:** 920430

**Impact:** Protocol-based attacks may bypass detection.

### Enclosed Alphanumerics in HTTP/2 (5 tests)

Rule 934120 does not detect enclosed alphanumerics (ⓐ, ⓑ, etc.) in HTTP/2 requests. Works correctly in HTTP/1.1.

**Affected rule:** 934120

**Impact:** Node.js encoding bypass attacks may succeed in HTTP/2.

### Multiphase Evaluation (2 tests)

Some rules fail only when multiphase evaluation is enabled.

**Affected rules:** 932300, 933120

**Impact:** RCE and PHP injection attacks may bypass detection with multiphase evaluation.

---

## Under Investigation (19 tests)

These require further analysis to determine appropriate action:

- **Behavioral differences (3 tests):** Filter blocks attacks instead of Envoy (still blocked, different layer)
- **Cookie detection (2 tests):** $Version parameter not matched
- **match_regex behavior (3 tests):** Different error message patterns
- **Early detection (1 test):** Detected by earlier rule instead of expected rule
- **XSS edge cases (2 tests):** TinyGo regex and HTML entity decoding issues
- **Java deserialization (1 test):** Pattern not detected
- **PL4 false positives (2 tests):** Benign requests trigger anomaly score
- **retry_once mechanism (5 tests):** Not working in current environment

---

## Version Information

- **Coraza**: v3.3.3
- **CoreRuleSet**: v4.24.1
- **Envoy/Istio**: WASM filter environment
- **coraza-proxy-wasm**: Latest
- **Documentation Date**: 2026-03-17
- **Test Coverage**: 94% pass rate (190 ignored / ~3,300 total)

For configuration details of ignored tests, see [test/conformance/ftw.yml](test/conformance/ftw.yml).

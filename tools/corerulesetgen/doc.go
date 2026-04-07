// Package corerulesetgen turns OWASP CoreRuleSet rule files on disk into Kubernetes
// RuleSource objects (type: Rule and type: Data) and a RuleSet manifest matching the
// operator's v1alpha1 API. It is intentionally a client-side generator only: it does
// not compile or validate Coraza rules.
//
// The composable pipeline is [ParseCRSVersion], [Scan], [Build], [WriteManifests]. [Generate]
// applies defaults, parses the version, validates the rules directory, scans, builds with
// stderr progress, and writes to the provided [io.Writer]; use it from CLIs. For tests or
// custom orchestration, call those functions directly.
package corerulesetgen

//go:build integration

/*
Copyright Coraza Kubernetes Operator contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package integration

import (
	"net/url"
	"path/filepath"
	"testing"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

// TestSampleRulesetBlocksSQLiAndXSS exercises the committed sample rules so
// their block action is backed by the default deny configured in base-rules.
func TestSampleRulesetBlocksSQLiAndXSS(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("sample-rules")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("apply sample rules")
	s.ApplyManifest(ns, filepath.Join("..", "..", "config", "samples", "ruleset.yaml"))
	s.ExpectRuleSetReady(ns, "default-ruleset")

	s.Step("create engine")
	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "default-ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.Step("deploy echo backend")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")

	s.Step("verify sample rules enforce")
	gw.ExpectAllowed("/?q=hello")
	gw.ExpectBlocked("/?q=evilmonkey")
	gw.ExpectBlocked("/?q=evildata")
	gw.ExpectBlocked("/?q=" + url.QueryEscape("select * from users"))
	gw.ExpectBlocked("/?q=" + url.QueryEscape("<script>alert(1)</script>"))
}

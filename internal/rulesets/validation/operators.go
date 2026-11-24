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

package validation

import (
	"fmt"

	"github.com/corazawaf/coraza/v3/experimental/plugins"
	"github.com/corazawaf/coraza/v3/experimental/plugins/plugintypes"
)

// gatewayModuleCheckedAt is the coraza-rs commit (CORAZA_RS_VERSION in the
// Makefile) the lists of what envoy-coraza supports in this package were
// checked against: unsupportedOperators, supportedOperators, operatorAliases,
// supportedDirectives and ctlDirectives. A test fails when the Makefile pin
// moves, so bumping the gateway module forces a re-check of the lists.
const gatewayModuleCheckedAt = "1caf0a5ae9f7ee0cf83899bb58daa6a33a98bff1"

// unsupportedOperators are operators Go Coraza registers but envoy-coraza is
// built without: it uses the coraza crate's default features, which leave out
// inspect-file, rbl and validate-schema, so a rule using one fails to load in
// the gateway as an unknown operator.
var unsupportedOperators = []string{"inspectFile", "rbl", "validateSchema"}

// init replaces each unsupported operator with a factory that fails, so every
// validation WAF rejects it wherever Coraza's parser resolves an operator:
// negated, in a chained rule, or in an included RuleData file. Coraza's own
// registrations run first, as an imported package is initialised before its
// importer.
func init() {
	for _, name := range unsupportedOperators {
		plugins.RegisterOperator(name, func(plugintypes.OperatorOptions) (plugintypes.Operator, error) {
			return nil, fmt.Errorf("@%s is not supported by the envoy-coraza plugin", name)
		})
	}
}

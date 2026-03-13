//go:build conformance

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

package conformance

import (
	"fmt"
	"os"
	"testing"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

// fw is the test framework instance, available to all tests in this package.
var fw *framework.Framework

func TestMain(m *testing.M) {
	var err error
	fw, err = framework.New()
	if err != nil {
		panic(fmt.Sprintf("failed to initialize test framework for conformance: %v", err))
	}
	os.Exit(m.Run())
}

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

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

func TestRuleDataCELRejectsReservedFileNames(t *testing.T) {
	tests := []struct {
		name     string
		object   string
		fileName string
	}{
		{name: "current directory", object: "cel-reserved-dot", fileName: "."},
		{name: "parent directory", object: "cel-reserved-dotdot", fileName: ".."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			data := utils.NewTestRuleData(tt.object, testNamespace, map[string]string{tt.fileName: "x"})

			err := k8sClient.Create(ctx, data)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not '.' or '..'")
		})
	}
}

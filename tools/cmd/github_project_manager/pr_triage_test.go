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

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputePRAreaLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		files  []string
		want   []string
	}{
		{
			name:   "api changes",
			labels: []string{},
			files:  []string{"api/v1alpha1/engine_types.go"},
			want:   []string{"area/api"},
		},
		{
			name:   "controller changes",
			labels: []string{},
			files:  []string{"internal/controller/engine_controller.go"},
			want:   []string{"area/controllers"},
		},
		{
			name:   "cache changes",
			labels: []string{},
			files:  []string{"internal/rulesets/cache/cache.go"},
			want:   []string{"area/cache"},
		},
		{
			name:   "test changes",
			labels: []string{},
			files:  []string{"test/integration/reconcile_test.go"},
			want:   []string{"area/testing"},
		},
		{
			name:   "infrastructure changes",
			labels: []string{},
			files:  []string{".github/workflows/ci.yml", "Makefile"},
			want:   []string{"area/infrastructure"},
		},
		{
			name:   "documentation changes",
			labels: []string{},
			files:  []string{"README.md", "CONTRIBUTING.md"},
			want:   []string{"area/documentation"},
		},
		{
			name:   "helm changes",
			labels: []string{},
			files:  []string{"charts/coraza-kubernetes-operator/values.yaml"},
			want:   []string{"area/helm"},
		},
		{
			name:   "multiple areas",
			labels: []string{},
			files:  []string{"api/v1alpha1/engine_types.go", "charts/coraza-kubernetes-operator/values.yaml", "test/integration/foo_test.go"},
			want:   []string{"area/api", "area/testing", "area/helm"},
		},
		{
			name:   "skips when area label already exists",
			labels: []string{"area/api"},
			files:  []string{"internal/controller/engine_controller.go"},
			want:   nil,
		},
		{
			name:   "no matching files",
			labels: []string{},
			files:  []string{"cmd/main.go"},
			want:   nil,
		},
		{
			name:   "deduplicates areas",
			labels: []string{},
			files:  []string{"test/integration/a_test.go", "test/e2e/b_test.go"},
			want:   []string{"area/testing"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, computePRAreaLabels(tt.labels, tt.files))
		})
	}
}

func TestComputePRSizeLabel(t *testing.T) {
	tests := []struct {
		name      string
		labels    []string
		additions int
		deletions int
		want      []string
	}{
		{
			name:      "XS for tiny changes",
			labels:    []string{},
			additions: 3,
			deletions: 2,
			want:      []string{"size/XS"},
		},
		{
			name:      "S for small changes",
			labels:    []string{},
			additions: 20,
			deletions: 15,
			want:      []string{"size/S"},
		},
		{
			name:      "M for medium changes",
			labels:    []string{},
			additions: 100,
			deletions: 50,
			want:      []string{"size/M"},
		},
		{
			name:      "L for large changes",
			labels:    []string{},
			additions: 300,
			deletions: 100,
			want:      []string{"size/L"},
		},
		{
			name:      "XL for very large changes",
			labels:    []string{},
			additions: 400,
			deletions: 200,
			want:      []string{"size/XL"},
		},
		{
			name:      "skips when size label already exists",
			labels:    []string{"size/M"},
			additions: 5,
			deletions: 0,
			want:      nil,
		},
		{
			name:      "boundary XS/S at 10",
			labels:    []string{},
			additions: 10,
			deletions: 0,
			want:      []string{"size/XS"},
		},
		{
			name:      "boundary S at 11",
			labels:    []string{},
			additions: 11,
			deletions: 0,
			want:      []string{"size/S"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, computePRSizeLabel(tt.labels, tt.additions, tt.deletions))
		})
	}
}

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
	}{
		{"1.2.3", true},
		{"v1.2.3", true},
		{"0.10.0", true},
		{"v1.0.0-rc.1", true},
		{"not-semver", false},
		{"1.2", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			v, ok := parseSemver(tt.input)
			assert.Equal(t, tt.ok, ok, "ok")
			if ok {
				assert.NotNil(t, v)
			}
		})
	}
}

func TestFindLowestMilestone(t *testing.T) {
	tests := []struct {
		name       string
		milestones []Milestone
		wantTitle  string
		wantErr    bool
	}{
		{
			name: "finds lowest semver",
			milestones: []Milestone{
				{Number: 3, Title: "v1.2.0"},
				{Number: 1, Title: "v1.0.0"},
				{Number: 2, Title: "v1.1.0"},
			},
			wantTitle: "v1.0.0",
		},
		{
			name: "skips non-semver titles",
			milestones: []Milestone{
				{Number: 1, Title: "backlog"},
				{Number: 2, Title: "v0.5.0"},
				{Number: 3, Title: "v0.3.1"},
			},
			wantTitle: "v0.3.1",
		},
		{
			name: "handles pre-release versions",
			milestones: []Milestone{
				{Number: 1, Title: "v2.0.0-rc.1"},
				{Number: 2, Title: "v1.5.0"},
			},
			wantTitle: "v1.5.0",
		},
		{
			name:       "no valid milestones returns error",
			milestones: []Milestone{{Number: 1, Title: "backlog"}},
			wantErr:    true,
		},
		{
			name:       "empty list returns error",
			milestones: []Milestone{},
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := findLowestMilestone(tt.milestones)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantTitle, m.Title)
		})
	}
}

func TestSemverLess(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"v1.0.0", "v2.0.0", true},
		{"v2.0.0", "v1.0.0", false},
		{"v1.0.0", "v1.1.0", true},
		{"v1.1.0", "v1.0.0", false},
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.1", "v1.0.0", false},
		{"v1.0.0", "v1.0.0", false},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_vs_"+tt.b, func(t *testing.T) {
			assert.Equal(t, tt.want, semverLess(tt.a, tt.b))
		})
	}
}

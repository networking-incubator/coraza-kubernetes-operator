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

package assemble_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
)

func TestAssembleOrder(t *testing.T) {
	tests := []struct {
		name    string
		sources []assemble.SourceInput
		want    string
	}{
		{
			name:    "single source is byte-exact",
			sources: []assemble.SourceInput{{Name: "only", Rules: "SecRuleEngine On\n"}},
			want:    "SecRuleEngine On\n",
		},
		{
			name: "sources are concatenated in list order",
			sources: []assemble.SourceInput{
				{Name: "second-listed", Rules: "SecRule B"},
				{Name: "first-listed", Rules: "SecRule A"},
				{Name: "third-listed", Rules: "SecRule C"},
			},
			want: "SecRule B\nSecRule A\nSecRule C",
		},
		{
			name: "an empty fragment still contributes its separator",
			sources: []assemble.SourceInput{
				{Name: "a", Rules: "SecRule A"},
				{Name: "empty", Rules: ""},
				{Name: "c", Rules: "SecRule C"},
			},
			want: "SecRule A\n\nSecRule C",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := assemble.Assemble(assemble.Inputs{Sources: tt.sources})
			require.NoError(t, err)
			assert.Equal(t, tt.want, content.Rules)
		})
	}
}

func TestAssembleDataMergeLastListedWins(t *testing.T) {
	content, err := assemble.Assemble(assemble.Inputs{
		Data: []assemble.DataInput{
			{Name: "first", Files: map[string]string{"a.txt": "a-first", "shared.txt": "first"}},
			{Name: "second", Files: map[string]string{"b.txt": "b-second", "shared.txt": "second"}},
			{Name: "third", Files: map[string]string{"shared.txt": "third"}},
		},
		Sources: []assemble.SourceInput{{Name: "rules", Rules: "SecRuleEngine On"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "a-first", string(content.DataFiles["a.txt"]))
	assert.Equal(t, "b-second", string(content.DataFiles["b.txt"]))
	assert.Equal(t, "third", string(content.DataFiles["shared.txt"]))
	assert.Len(t, content.DataFiles, 3)
}

func TestAssembleNoDataReturnsNilMap(t *testing.T) {
	content, err := assemble.Assemble(assemble.Inputs{
		Sources: []assemble.SourceInput{{Name: "rules", Rules: "SecRuleEngine On"}},
	})
	require.NoError(t, err)
	assert.Nil(t, content.DataFiles)
}

func TestAssembleDuplicateReferences(t *testing.T) {
	tests := []struct {
		name    string
		inputs  assemble.Inputs
		wantErr string
	}{
		{
			name: "duplicate RuleSource names are sorted",
			inputs: assemble.Inputs{
				Sources: []assemble.SourceInput{
					{Name: "z", Rules: "one"},
					{Name: "a", Rules: "two"},
					{Name: "z", Rules: "three"},
					{Name: "a", Rules: "four"},
				},
			},
			wantErr: "spec.sources contains duplicate RuleSource name(s): a, z",
		},
		{
			name: "duplicate RuleData names",
			inputs: assemble.Inputs{
				Sources: []assemble.SourceInput{{Name: "rules", Rules: "SecRuleEngine On"}},
				Data: []assemble.DataInput{
					{Name: "data", Files: map[string]string{"a": "1"}},
					{Name: "data", Files: map[string]string{"b": "2"}},
				},
			},
			wantErr: "spec.data contains duplicate RuleData name(s): data",
		},
		{
			name: "both lists report together",
			inputs: assemble.Inputs{
				Sources: []assemble.SourceInput{
					{Name: "rules", Rules: "one"},
					{Name: "rules", Rules: "two"},
				},
				Data: []assemble.DataInput{
					{Name: "data", Files: map[string]string{"a": "1"}},
					{Name: "data", Files: map[string]string{"b": "2"}},
				},
			},
			wantErr: "spec.sources contains duplicate RuleSource name(s): rules; spec.data contains duplicate RuleData name(s): data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := assemble.Assemble(tt.inputs)
			require.EqualError(t, err, tt.wantErr)
		})
	}
}

func TestAssembleEmptyRulesPolicy(t *testing.T) {
	tests := []struct {
		name      string
		policy    assemble.EmptyRulesPolicy
		sources   []assemble.SourceInput
		wantErr   error
		wantRules string
	}{
		{
			name:    "Reject is the default",
			sources: []assemble.SourceInput{{Name: "empty", Rules: ""}},
			wantErr: assemble.ErrEmptyRules,
		},
		{
			name:    "explicit Reject",
			policy:  assemble.EmptyRulesPolicyReject,
			sources: []assemble.SourceInput{{Name: "empty", Rules: ""}},
			wantErr: assemble.ErrEmptyRules,
		},
		{
			name:    "whitespace-only rules are empty for Reject",
			sources: []assemble.SourceInput{{Name: "empty", Rules: "\n  \t\n"}},
			wantErr: assemble.ErrEmptyRules,
		},
		{
			name:      "Allow accepts empty rules",
			policy:    assemble.EmptyRulesPolicyAllow,
			sources:   []assemble.SourceInput{{Name: "empty", Rules: ""}},
			wantRules: "",
		},
		{
			name:      "Allow preserves whitespace byte-exactly",
			policy:    assemble.EmptyRulesPolicyAllow,
			sources:   []assemble.SourceInput{{Name: "empty", Rules: "\n  \t\n"}},
			wantRules: "\n  \t\n",
		},
		{
			name:    "unknown policy is rejected",
			policy:  assemble.EmptyRulesPolicy("reject"),
			sources: []assemble.SourceInput{{Name: "rules", Rules: "SecRuleEngine On"}},
			wantErr: errors.New(`unknown empty rules policy "reject"`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := assemble.Assemble(assemble.Inputs{
				EmptyRulesPolicy: tt.policy,
				Sources:          tt.sources,
			})
			if tt.wantErr != nil {
				require.Error(t, err)
				if errors.Is(tt.wantErr, assemble.ErrEmptyRules) {
					assert.ErrorIs(t, err, assemble.ErrEmptyRules)
				} else {
					assert.EqualError(t, err, tt.wantErr.Error())
				}
				assert.Empty(t, content.Rules)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantRules, content.Rules)
		})
	}
}

func TestAssembleValidatesPMFromFile(t *testing.T) {
	t.Run("missing data is an error", func(t *testing.T) {
		_, err := assemble.Assemble(assemble.Inputs{
			Sources: []assemble.SourceInput{{
				Name:  "rules",
				Rules: `SecRule ARGS "@pmFromFile blocked.txt" "id:1,phase:2,deny"`,
			}},
		})
		require.ErrorContains(t, err, "@pmFromFile blocked.txt requires spec.data RuleData but none are referenced")
	})

	t.Run("referenced data is accepted", func(t *testing.T) {
		content, err := assemble.Assemble(assemble.Inputs{
			Sources: []assemble.SourceInput{{
				Name:  "rules",
				Rules: `SecRule ARGS "@pmFromFile blocked.txt" "id:1,phase:2,deny"`,
			}},
			Data: []assemble.DataInput{{
				Name:  "data",
				Files: map[string]string{"blocked.txt": "attack\n"},
			}},
		})
		require.NoError(t, err)
		assert.Equal(t, "attack\n", string(content.DataFiles["blocked.txt"]))
	})

	t.Run("documentation comments do not require data", func(t *testing.T) {
		_, err := assemble.Assemble(assemble.Inputs{
			Sources: []assemble.SourceInput{{
				Name:  "rules",
				Rules: "# @pmFromFile optional.txt is only documentation\nSecRuleEngine On",
			}},
		})
		require.NoError(t, err)
	})
}

func TestFingerprint(t *testing.T) {
	base := assemble.Inputs{
		RuleSetUID:  types.UID("ruleset-uid"),
		RuleSetName: "ruleset",
		Namespace:   "shop",
		Sources: []assemble.SourceInput{
			{Name: "a", UID: types.UID("a-uid"), Generation: 1, Rules: "SecRuleEngine On"},
		},
		Data: []assemble.DataInput{
			{Name: "d", UID: types.UID("d-uid"), Generation: 1, Files: map[string]string{"a.txt": "a"}},
		},
	}

	first := assemble.Fingerprint(base)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, first)
	assert.Equal(t, first, assemble.Fingerprint(base), "fingerprint must be deterministic")

	metadataOnly := base
	metadataOnly.Sources = append([]assemble.SourceInput(nil), base.Sources...)
	metadataOnly.Sources[0].UID = types.UID("recreated-uid")
	metadataOnly.Sources[0].Generation = 7
	assert.NotEqual(t, first, assemble.Fingerprint(metadataOnly), "input identity includes object metadata")

	contentChanged := base
	contentChanged.Sources = append([]assemble.SourceInput(nil), base.Sources...)
	contentChanged.Sources[0].Rules = "SecRuleEngine On "
	assert.NotEqual(t, first, assemble.Fingerprint(contentChanged))
}

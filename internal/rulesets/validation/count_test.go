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
	"cmp"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCountRules(t *testing.T) {
	var thousand strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&thousand, "SecAction \"id:%d,phase:1,pass,nolog\"\n", i+1)
	}
	tests := []struct {
		name  string
		rules string
		data  map[string][]byte
		limit int
		want  int
	}{
		{name: "chain members count", rules: "SecRule ARGS \"@rx a\" \"id:1,phase:1,deny,chain\"\nSecRule ARGS \"@rx b\" \"t:none\"", want: 2},
		{name: "continuation lines are one directive", rules: "SecRule ARGS \"@rx a\" \\\n    \"id:1,phase:1,deny\"\nSecAction \\\n    \"id:2,phase:1,pass\"", want: 2},
		{name: "directive names are case-insensitive", rules: "secrule ARGS \"@rx a\" \"id:1\"\nSECACTION \"id:2\"", want: 2},
		{name: "markers count, comments and other directives do not", rules: "# SecRule ARGS \"@rx a\" \"id:1\"\nSecRuleEngine On\nSecRuleRemoveById 1\nSecMarker END\n" + strings.Repeat("SecMarker M\n", 999), want: 1000},
		{
			name:  "Included RuleData files count, data files do not",
			rules: "Include rules.conf\nSecRule ARGS \"@pmFromFile words.data\" \"id:1\"",
			data: map[string][]byte{
				"rules.conf": []byte("SecAction \"id:2\"\nSecAction \"id:3\""),
				"words.data": []byte("SecRule\nSecAction \"id:4\""),
			},
			want: 3,
		},
		{
			name:  "a file counts at every Include, whatever its spelling",
			rules: "Include r.conf\nInclude ./r.conf\nInclude \".//r.conf\"",
			data:  map[string][]byte{"r.conf": []byte("SecAction \"id:1\"")},
			want:  3,
		},
		{
			name:  "a file of 1000 rules Included 100 times",
			rules: strings.Repeat("Include r.conf\n", 100),
			data:  map[string][]byte{"r.conf": []byte(thousand.String())},
			limit: 200000,
			want:  100000,
		},
		{
			name:  "Includes past Coraza's limit are not followed",
			rules: strings.Repeat("Include r.conf\n", 150),
			data:  map[string][]byte{"r.conf": []byte("SecAction \"id:1\"")},
			want:  100,
		},
		{
			name:  "counting stops past the limit",
			rules: thousand.String(),
			limit: 10,
			want:  11,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, CountRules(tt.rules, tt.data, cmp.Or(tt.limit, 1<<30)))
		})
	}
}

// TestCountRulesCountsCRSTuningStandIns counts the stand-in rule the
// validation WAF defines for every CRS rule a tuning directive addresses,
// when the module embeds CRS.
func TestCountRulesCountsCRSTuningStandIns(t *testing.T) {
	previous := BundledCRSVersion
	BundledCRSVersion = "4.28.0"
	t.Cleanup(func() { BundledCRSVersion = previous })

	var tuning strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&tuning, "SecRuleUpdateTargetById %d \"!ARGS:foo\"\n", 900000+i)
	}
	assert.Equal(t, 1000, CountRules("Include crs/rules/*.conf\n"+tuning.String(), nil, 1<<30))
	assert.Equal(t, 1000, CountRules("Include crs/rules/*.conf\nInclude tuning.conf", map[string][]byte{"tuning.conf": []byte(tuning.String())}, 1<<30))
	assert.Equal(t, 1000, CountRules("Include crs/rules/*.conf\n"+tuning.String(), nil, 999), "counting stops past the limit")
}

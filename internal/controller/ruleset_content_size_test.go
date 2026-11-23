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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/validation"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

// testFiles returns count RuleData files of size bytes each.
func testFiles(prefix string, count, size int) map[string]string {
	files := make(map[string]string, count)
	for i := range count {
		files[fmt.Sprintf("%s%d.data", prefix, i)] = strings.Repeat("a", size)
	}
	return files
}

func TestContentSize(t *testing.T) {
	budget := render.ContentBudget
	source := func(name, rules string) assemble.SourceInput {
		return assemble.SourceInput{Name: name, Rules: rules}
	}
	// fill is a RuleData with one file whose entry, "a.data":"...", encodes
	// to n bytes.
	fill := func(name string, n int) assemble.DataInput {
		return assemble.DataInput{Name: name, Files: map[string]string{"a.data": strings.Repeat("a", n-len(`"a.data":""`))}}
	}
	// kubectl coraza generate coreruleset emits, for CRS v4.28.0, 28
	// RuleSources with 722740 bytes of rule text and one RuleData with 21
	// files of 171356 bytes.
	crs := assemble.Inputs{Data: []assemble.DataInput{{Name: "crs-data", Files: testFiles("crs-", 21, 171356/21+1)}}}
	for i := range 28 {
		crs.Sources = append(crs.Sources, source(fmt.Sprintf("crs-%d", i), strings.Repeat("#", 722740/28+1)))
	}
	tests := []struct {
		name    string
		inputs  assemble.Inputs
		wantErr string // empty when accepted
	}{
		{
			name: "exactly the budget",
			inputs: assemble.Inputs{
				Sources: []assemble.SourceInput{source("rules", strings.Repeat("#", 1024))},
				Data:    []assemble.DataInput{fill("files", budget-1024)},
			},
		},
		{
			name: "one byte past the budget",
			inputs: assemble.Inputs{
				Sources: []assemble.SourceInput{source("rules", strings.Repeat("#", 1025))},
				Data:    []assemble.DataInput{fill("files", budget-1024)},
			},
			wantErr: fmt.Sprintf("encode to %d bytes at RuleSource rules, above the %d bytes one configuration can carry; the largest input is RuleData files with %d bytes",
				budget+1, budget, budget-1024),
		},
		{
			name:    "escaped rule text past the budget although its raw size is not",
			inputs:  assemble.Inputs{Sources: []assemble.SourceInput{source("escaped", strings.Repeat(`<">`, budget/12))}},
			wantErr: "at RuleSource escaped",
		},
		{
			name:    "stops at the first source past the budget",
			inputs:  assemble.Inputs{Sources: []assemble.SourceInput{source("first", strings.Repeat("#", budget)), source("second", "#"), source("third", "#")}},
			wantErr: "at RuleSource second",
		},
		{
			name: "a later RuleData replaces an earlier file",
			inputs: assemble.Inputs{
				Sources: []assemble.SourceInput{source("rules", "#")},
				Data:    []assemble.DataInput{fill("earlier", budget), fill("later", 12)},
			},
		},
		{name: "the generated CRS RuleSet", inputs: crs},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkInputsSize(tt.inputs)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, errContentTooLarge)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestContentSizeMatchesRenderedSize renders escaped rule text and merged
// RuleData for an Engine with the longest name and namespace: the runtime
// configuration is exactly the counted size plus the rest of the document, so
// content within the budget always renders within the plugin's limit.
func TestContentSizeMatchesRenderedSize(t *testing.T) {
	sources := []*wafv1alpha1.RuleSource{
		utils.NewTestRuleSource("first", testNamespace, "SecRule ARGS \"@rx <a&b>\" \"id:1,phase:1,pass,msg:'é\\\\'\"\n# a\tcomment"),
		utils.NewTestRuleSource("second", testNamespace, "SecAction \"id:2,phase:1,pass,nolog\"\u2028"),
	}
	data := []*wafv1alpha1.RuleData{
		utils.NewTestRuleData("earlier", testNamespace, map[string]string{"x.data": "<>&\"\\\n", "y.data": "plain"}),
		utils.NewTestRuleData("later", testNamespace, map[string]string{"x.data": "replaced é"}),
	}
	var size contentSize
	inputs := assemble.Inputs{RuleSetName: "rules", Namespace: testNamespace}
	for _, d := range slices.Backward(data) {
		require.NoError(t, size.addData(d.Name, d.Spec.Files))
	}
	for _, d := range data {
		inputs.Data = append(inputs.Data, assemble.DataInput{Name: d.Name, Files: d.Spec.Files})
	}
	for _, s := range sources {
		require.NoError(t, size.addSource(s.Name, s.Spec.Rules))
		inputs.Sources = append(inputs.Sources, assemble.SourceInput{Name: s.Name, Rules: s.Spec.Rules})
	}
	content, err := assemble.Assemble(inputs)
	require.NoError(t, err)
	contentHash, err := render.RuleSetContentHash(content)
	require.NoError(t, err)

	artifact, err := render.Render(render.EngineInput{
		Namespace:        strings.Repeat("n", 63),
		Name:             strings.Repeat("e", 253),
		UID:              "uuuuuuuu-uuuu-uuuu-uuuu-uuuuuuuuuuuu",
		RuleSet:          render.RuleSetAccepted{UID: "ruleset", ContentHash: contentHash},
		Content:          content,
		EmptyRulesPolicy: string(assemble.EmptyRulesPolicyReject),
		FailurePolicy:    string(wafv1alpha1.FailurePolicyAllow),
		Binary:           render.BinaryRef{URL: testAssetBaseURL + "/wasm/" + testSHA256 + ".wasm", SHA256: testSHA256},
	})
	require.NoError(t, err)
	var envelope struct {
		ConfigGzipB64 string `json:"config_gzip_b64"`
	}
	require.NoError(t, json.Unmarshal(artifact.Payload, &envelope))
	compressed, err := base64.StdEncoding.DecodeString(envelope.ConfigGzipB64)
	require.NoError(t, err)
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	require.NoError(t, err)
	document, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, render.MaxConfigurationBytes-render.ContentBudget+size.encoded, len(document))
}

// TestRuleSetReconciler_BoundsContentBeforeAssembly accepts content that
// encodes to exactly what one configuration can carry, with the most RuleData
// the default limits allow, then refuses one byte more before the content is
// assembled or compiled, keeping the content accepted before.
func TestRuleSetReconciler_BoundsContentBeforeAssembly(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)

	// The edit adds one byte and references a file no RuleData holds:
	// assembling it would report InvalidRuleSet, so ContentTooLarge shows the
	// content was refused before assembly and before any Coraza compile.
	rules := `SecRule ARGS "@contains missing.data1" "id:1,phase:2,deny,status:403"`
	grown := `SecRule ARGS "@pmFromFile missing.data" "id:1,phase:2,deny,status:403"`
	require.Len(t, grown, len(rules)+1)
	// commentLines returns comment lines that encode to exactly n bytes.
	commentLines := func(n int) string {
		var b strings.Builder
		for ; n >= 1000; n -= 1000 {
			b.WriteString("#" + strings.Repeat("a", 997) + "\n")
		}
		if n > 0 {
			b.WriteString("#" + strings.Repeat("a", n-1))
		}
		return b.String()
	}

	// Six RuleData of 1 MiB reach the default decoded RuleData limit, and
	// comment-only RuleSources fill the rest of the budget.
	var size contentSize
	refs := make([]wafv1alpha1.DataReference, 6)
	for i := range refs {
		refs[i].Name = fmt.Sprintf("bound-data-%d", i)
		data := utils.NewTestRuleData(refs[i].Name, ns, map[string]string{refs[i].Name + ".data": strings.Repeat("a", 1<<20)})
		require.NoError(t, k8sClient.Create(ctx, data))
		require.NoError(t, size.addData(data.Name, data.Spec.Files))
	}
	source := utils.NewTestRuleSource("bound-src", ns, rules)
	require.NoError(t, k8sClient.Create(ctx, source))
	require.NoError(t, size.addSource(source.Name, source.Spec.Rules))
	sources := []wafv1alpha1.SourceReference{{Name: source.Name}}
	for i := 0; size.encoded < render.ContentBudget; i++ {
		filler := utils.NewTestRuleSource(fmt.Sprintf("bound-fill-%d", i), ns, commentLines(min(render.ContentBudget-size.encoded-2, 1<<20)))
		require.NoError(t, k8sClient.Create(ctx, filler))
		require.NoError(t, size.addSource(filler.Name, filler.Spec.Rules))
		sources = append(sources, wafv1alpha1.SourceReference{Name: filler.Name})
	}
	require.Equal(t, render.ContentBudget, size.encoded)

	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "bound-ruleset",
		Namespace: ns,
		Sources:   sources,
		Data:      refs,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))

	reconciler := &RuleSetReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: utils.NewFakeRecorder(),
		Cache:    cache.NewRuleSetCache(),
	}
	nn := client.ObjectKeyFromObject(ruleSet)
	_, err := reconcileRuleSetWithRuleSources(ctx, t, nn, reconciler)
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
	require.NotNil(t, ruleSet.Status.AcceptedContent, "content of exactly the budget must be accepted")
	accepted := *ruleSet.Status.AcceptedContent

	editRuleSetSource(t, ctx, source, grown, ruleSet, reconciler)

	degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, conditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, metav1.ConditionTrue, degraded.Status)
	assert.Equal(t, ruleSetDegradedReasonContentTooLarge, degraded.Reason)
	assert.Contains(t, degraded.Message, fmt.Sprintf("encode to %d bytes", render.ContentBudget+1))
	require.NotNil(t, ruleSet.Status.AcceptedContent, "a refused edit must keep the accepted content")
	assert.Equal(t, accepted, *ruleSet.Status.AcceptedContent)
}

// TestLoadersCountOnlyMergedRuleData lists RuleData whose files, counted
// twice, would pass the delivery limit, while the merged files that are
// delivered stay far below it: both loaders accept it and keep spec order.
func TestLoadersCountOnlyMergedRuleData(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)

	rules := `SecRule ARGS "@pmFromFile f0.data" "id:1,phase:2,deny,status:403"`
	require.NoError(t, k8sClient.Create(ctx, utils.NewTestRuleSource("merged-src", ns, rules)))
	// Eight 1 MiB files that the last RuleData replaces with small ones.
	refs := make([]wafv1alpha1.DataReference, 9)
	replaced := make(map[string]string, 8)
	for i := range 8 {
		file := fmt.Sprintf("f%d.data", i)
		replaced[file] = "attack\n"
		refs[i].Name = fmt.Sprintf("merged-data-%d", i)
		require.NoError(t, k8sClient.Create(ctx, utils.NewTestRuleData(refs[i].Name, ns, map[string]string{file: strings.Repeat("a", 1<<20)})))
	}
	refs[8].Name = "merged-data-last"
	require.NoError(t, k8sClient.Create(ctx, utils.NewTestRuleData(refs[8].Name, ns, replaced)))
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "merged-ruleset",
		Namespace: ns,
		Sources:   []wafv1alpha1.SourceReference{{Name: "merged-src"}},
		Data:      refs,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))

	inputs, err := loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)
	names := make([]string, len(inputs.Data))
	for i, data := range inputs.Data {
		names[i] = data.Name
	}
	wantNames := make([]string, len(refs))
	for i, ref := range refs {
		wantNames[i] = ref.Name
	}
	assert.Equal(t, wantNames, names, "inputs must keep spec order")

	_, err = reconcileRuleSetWithRuleSources(ctx, t, client.ObjectKeyFromObject(ruleSet), &RuleSetReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: utils.NewTestRecorder(),
		Cache:    cache.NewRuleSetCache(),
	})
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(ruleSet), ruleSet))
	assert.NotNil(t, ruleSet.Status.AcceptedContent, "content within the limit once merged must be accepted")
}

// TestContentPastDeliveryLimitKeepsServingAcceptedContent grows the RuleData
// of an accepted RuleSet past a delivery limit. The Engine and the publication
// refuse the new content, the RuleSet reports ContentTooLarge, and the accepted
// configuration and its published snapshot keep serving.
func TestContentPastDeliveryLimitKeepsServingAcceptedContent(t *testing.T) {
	oneMiB := func(i int) map[string]string {
		return map[string]string{fmt.Sprintf("f%d.data", i): strings.Repeat("a", 1<<20)}
	}
	tests := []struct {
		name    string
		data    int
		grow    func(i int) map[string]string
		message string
	}{
		{name: "past what one configuration can carry", data: 8, grow: oneMiB, message: "above the 8388004 bytes one configuration can carry"},
		{name: "RuleData between the decoded limit and the configuration limit", data: 7, grow: oneMiB, message: "decoded RuleData size 7340032 exceeds the limit of 6291456"},
		{name: "merged RuleData files past the file limit", data: 2, grow: func(i int) map[string]string {
			files := make(map[string]string, 600)
			for j := range 600 {
				files[fmt.Sprintf("d%d-%d.data", i, j)] = "x\n"
			}
			return files
		}, message: "RuleData file count 1200 exceeds the limit of 1024"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ns := setupTestNamespace(t, ctx)
			createTestGateway(t, ctx, k8sClient, "limit-gw", ns)

			data := make([]*wafv1alpha1.RuleData, tt.data)
			refs := make([]wafv1alpha1.DataReference, len(data))
			for i := range data {
				data[i] = utils.NewTestRuleData(fmt.Sprintf("limit-data-%d", i), ns, map[string]string{fmt.Sprintf("f%d.data", i): "attack\n"})
				require.NoError(t, k8sClient.Create(ctx, data[i]))
				refs[i].Name = data[i].Name
			}
			ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
				Name:      "limit-ruleset",
				Namespace: ns,
				Sources:   []wafv1alpha1.SourceReference{{Name: "limit-src"}},
				Data:      refs,
			})
			require.NoError(t, k8sClient.Create(ctx, ruleSet))
			acceptTestRuleSet(t, ctx, ruleSet, testAcceptRuleText)
			acceptedContent := *ruleSet.Status.AcceptedContent

			engine := utils.NewTestEngine(utils.EngineOptions{
				Name:        "limit-engine",
				Namespace:   ns,
				GatewayName: "limit-gw",
				RuleSetName: ruleSet.Name,
				WasmURL:     utils.DefaultTestWasmURL,
				WasmSHA256:  utils.DefaultTestWasmSHA256,
			})
			require.NoError(t, k8sClient.Create(ctx, engine))
			engineReconciler := newAcceptedEngineTestReconciler()
			reconcileEngine(t, ctx, engineReconciler, engine)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(engine)}
			require.NoError(t, k8sClient.Get(ctx, request.NamespacedName, engine))
			accepted := engine.Status.AcceptedConfiguration
			require.NotNil(t, accepted)

			publication := newPublicationReconciler(t)
			_, err := publication.Reconcile(ctx, request)
			require.NoError(t, err)
			served, ok := publication.Store.Get(request.String())
			require.True(t, ok)

			for i, d := range data {
				require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(d), d))
				d.Spec.Files = tt.grow(i)
				require.NoError(t, k8sClient.Update(ctx, d))
			}
			requireAcceptedKept := func(t *testing.T, conditionType, reason string) {
				t.Helper()
				var current wafv1alpha1.Engine
				require.NoError(t, k8sClient.Get(ctx, request.NamespacedName, &current))
				assert.Equal(t, accepted, current.Status.AcceptedConfiguration, "a refused edit must keep the accepted configuration")
				condition := apimeta.FindStatusCondition(current.Status.Conditions, conditionType)
				require.NotNil(t, condition)
				assert.Equal(t, reason, condition.Reason)
			}

			// The Engine and the publication see the inputs before the RuleSet does.
			_, err = engineReconciler.Reconcile(ctx, request)
			require.NoError(t, err)
			requireAcceptedKept(t, conditionProgressing, reasonAwaitingRuleSetAcceptance)
			_, err = publication.Reconcile(ctx, request)
			require.NoError(t, err, "content past a limit is not retried as a transient error")
			snapshot, ok := publication.Store.Get(request.String())
			require.True(t, ok)
			assert.Equal(t, served.Version, snapshot.Version, "the published snapshot must keep serving")

			_, err = reconcileRuleSetWithRuleSources(ctx, t, client.ObjectKeyFromObject(ruleSet), &RuleSetReconciler{
				Client:   k8sClient,
				Scheme:   scheme,
				Recorder: utils.NewTestRecorder(),
				Cache:    cache.NewRuleSetCache(),
			})
			require.NoError(t, err)
			var current wafv1alpha1.RuleSet
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(ruleSet), &current))
			degraded := apimeta.FindStatusCondition(current.Status.Conditions, conditionDegraded)
			require.NotNil(t, degraded)
			assert.Equal(t, ruleSetDegradedReasonContentTooLarge, degraded.Reason)
			assert.Contains(t, degraded.Message, tt.message)
			require.NotNil(t, current.Status.AcceptedContent)
			assert.Equal(t, acceptedContent, *current.Status.AcceptedContent)

			_, err = engineReconciler.Reconcile(ctx, request)
			require.NoError(t, err)
			requireAcceptedKept(t, conditionDegraded, "RuleSetDegraded")
		})
	}
}

// TestRuleSetReconciler_RefusesRulesPastTheLimit accepts a RuleSet at the rule
// limit and refuses an edit that adds one rule, keeping the accepted content.
func TestRuleSetReconciler_RefusesRulesPastTheLimit(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)
	rule := func(id int) string {
		return fmt.Sprintf("SecRule ARGS \"@contains attack%d\" \"id:%d,phase:2,deny,status:403\"\n", id, id)
	}
	source := utils.NewTestRuleSource("count-src", ns, rule(1)+rule(2)+rule(3))
	require.NoError(t, k8sClient.Create(ctx, source))
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "count-ruleset",
		Namespace: ns,
		Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
	})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	reconciler := &RuleSetReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: utils.NewFakeRecorder(),
		Cache:    cache.NewRuleSetCache(),
		MaxRules: 3,
	}
	_, err := reconcileRuleSetWithRuleSources(ctx, t, client.ObjectKeyFromObject(ruleSet), reconciler)
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(ruleSet), ruleSet))
	require.NotNil(t, ruleSet.Status.AcceptedContent, "a RuleSet at the rule limit must be accepted")
	accepted := *ruleSet.Status.AcceptedContent

	// A marker is a rule too.
	editRuleSetSource(t, ctx, source, rule(1)+rule(2)+rule(3)+"SecMarker END\n", ruleSet, reconciler)

	degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, conditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, ruleSetDegradedReasonContentTooLarge, degraded.Reason)
	assert.Contains(t, degraded.Message, "more than 3 SecRule, SecAction and SecMarker directives")
	require.NotNil(t, ruleSet.Status.AcceptedContent)
	assert.Equal(t, accepted, *ruleSet.Status.AcceptedContent)
}

// TestRuleSetReconciler_DoesNotRecompileRejectedContent rejects content whose
// sources compile one by one but not together, then reconciles it again: the
// RuleSet reuses the compile result instead of compiling the same content.
func TestRuleSetReconciler_DoesNotRecompileRejectedContent(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)
	rule := `SecRule ARGS "@contains attack" "id:7,phase:2,deny,status:403"`
	first := utils.NewTestRuleSource("dup-first", ns, rule)
	second := utils.NewTestRuleSource("dup-second", ns, rule)
	require.NoError(t, k8sClient.Create(ctx, first))
	require.NoError(t, k8sClient.Create(ctx, second))
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "dup-ruleset",
		Namespace: ns,
		Sources:   []wafv1alpha1.SourceReference{{Name: first.Name}, {Name: second.Name}},
	})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	reconciler := &RuleSetReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: utils.NewFakeRecorder(),
		Cache:    cache.NewRuleSetCache(),
	}
	nn := client.ObjectKeyFromObject(ruleSet)
	degradedMessage := func() string {
		t.Helper()
		_, err := reconcileRuleSetWithRuleSources(ctx, t, nn, reconciler)
		require.NoError(t, err)
		require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
		degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, conditionDegraded)
		require.NotNil(t, degraded)
		require.Equal(t, ruleSetDegradedReasonInvalidRuleSet, degraded.Reason)
		return degraded.Message
	}
	require.Contains(t, degradedMessage(), "duplicated rule id 7")

	// Swap the recorded result for a marker: a reconcile that compiled again
	// would report the duplicate rule ID instead.
	reconciler.resultsMu.Lock()
	results := reconciler.results[nn]
	require.True(t, results.compiled)
	results.compileErr = "recorded compile result"
	reconciler.results[nn] = results
	reconciler.resultsMu.Unlock()
	require.Contains(t, degradedMessage(), "recorded compile result")

	// Different content is compiled.
	editRuleSetSource(t, ctx, second, `SecRule ARGS "@contains other" "id:7,phase:2,deny,status:403"`, ruleSet, reconciler)
	require.Contains(t, degradedMessage(), "duplicated rule id 7")
}

// TestRuleDataAndRuleSetSchemaBounds checks that the API server enforces the
// CRD bounds: RuleData file count and size, and RuleSet reference counts.
func TestRuleDataAndRuleSetSchemaBounds(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)

	tests := []struct {
		name    string
		object  client.Object
		wantErr string
	}{
		{name: "1024 files", object: utils.NewTestRuleData("max-files", ns, testFiles("f", 1024, 1))},
		{name: "1025 files", object: utils.NewTestRuleData("too-many-files", ns, testFiles("f", 1025, 1)), wantErr: "spec.files: Too many"},
		{name: "a 1 MiB file", object: utils.NewTestRuleData("max-file", ns, testFiles("f", 1, 1<<20))},
		{name: "a file over 1 MiB", object: utils.NewTestRuleData("large-file", ns, testFiles("f", 1, 1<<20+1)), wantErr: "each file must be at most 1048576 characters"},
		{name: "256 sources and 64 data", object: minimalRuleSet(ns, "max-refs", 256, 64)},
		{name: "257 sources", object: minimalRuleSet(ns, "too-many-sources", 257, 0), wantErr: "spec.sources: Too many"},
		{name: "65 data", object: minimalRuleSet(ns, "too-many-data", 1, 65), wantErr: "spec.data: Too many"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := k8sClient.Create(ctx, tt.object)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.True(t, apierrors.IsInvalid(err), "want Invalid, got %v", err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestRuleSetReconciler_ContentLimits(t *testing.T) {
	var thousand strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&thousand, "SecAction \"id:%d,phase:1,pass,nolog\"\n", i+1)
	}
	tests := []struct {
		name    string
		content assemble.Content
		want    string
	}{
		{
			name: "a file of 1000 rules Included 100 times",
			content: assemble.Content{
				Rules:     strings.Repeat("Include r.conf\n", 100),
				DataFiles: map[string][]byte{"r.conf": []byte(thousand.String())},
			},
			want: "more than 20000 SecRule, SecAction and SecMarker directives",
		},
		{
			name: "RuleData past the decoded limit",
			content: assemble.Content{Rules: "#", DataFiles: func() map[string][]byte {
				files := make(map[string][]byte, 7)
				for name, data := range testFiles("f", 7, 1<<20) {
					files[name] = []byte(data)
				}
				return files
			}()},
			want: "decoded RuleData size 7340032 exceeds the limit of 6291456",
		},
		{name: "the generated CRS shape", content: assemble.Content{Rules: thousand.String()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (&RuleSetReconciler{}).limitsError(tt.content)
			if tt.want == "" {
				assert.Empty(t, got)
				return
			}
			assert.Contains(t, got, tt.want)
		})
	}
}

// TestRuleSetReconciler_ContentLimitsCountCRSTuning refuses CRS tuning past
// the rule limit: the validation WAF defines a stand-in rule for every CRS
// rule a tuning directive addresses when the module embeds CRS.
func TestRuleSetReconciler_ContentLimitsCountCRSTuning(t *testing.T) {
	previous := validation.BundledCRSVersion
	validation.BundledCRSVersion = "4.28.0"
	t.Cleanup(func() { validation.BundledCRSVersion = previous })

	var rules strings.Builder
	rules.WriteString("Include crs/rules/*.conf\n")
	for i := range 1000 {
		fmt.Fprintf(&rules, "SecRuleUpdateTargetById %d \"!ARGS:foo\"\n", 900000+i)
	}
	content := assemble.Content{Rules: rules.String()}
	assert.Empty(t, (&RuleSetReconciler{MaxRules: 1000}).limitsError(content))
	assert.Contains(t, (&RuleSetReconciler{MaxRules: 999}).limitsError(content), "more than 999 SecRule, SecAction and SecMarker directives")
}

// TestRuleSetReconciler_RefusesContentThatCompressesPastTheLimit fills the
// budget with high-entropy rule text: it encodes within what one configuration
// can carry but compresses past the plugin's limit, and the RuleSet refuses it
// once, then reuses the result.
func TestRuleSetReconciler_RefusesContentThatCompressesPastTheLimit(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)
	const alphabet = "!#$%'()*+,-./0123456789:;=?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[]^_abcdefghijklmnopqrstuvwxyz{|}~"
	random := rand.New(rand.NewPCG(1, 2))
	comments := func(n int) string {
		var b strings.Builder
		for b.Len() < n {
			b.WriteByte('#')
			for range 120 {
				b.WriteByte(alphabet[random.IntN(len(alphabet))])
			}
			b.WriteByte('\n')
		}
		return b.String()
	}
	sources := make([]wafv1alpha1.SourceReference, 6)
	for i := range sources {
		sources[i].Name = fmt.Sprintf("entropy-%d", i)
		require.NoError(t, k8sClient.Create(ctx, utils.NewTestRuleSource(sources[i].Name, ns, comments(render.ContentBudget/len(sources)*98/100))))
	}
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{Name: "entropy-ruleset", Namespace: ns, Sources: sources})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	reconciler := &RuleSetReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: utils.NewFakeRecorder(),
		Cache:    cache.NewRuleSetCache(),
	}
	nn := client.ObjectKeyFromObject(ruleSet)
	degradedMessage := func() string {
		t.Helper()
		_, err := reconcileRuleSetWithRuleSources(ctx, t, nn, reconciler)
		require.NoError(t, err)
		require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
		degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, conditionDegraded)
		require.NotNil(t, degraded, "%+v", ruleSet.Status.Conditions)
		require.Equal(t, ruleSetDegradedReasonContentTooLarge, degraded.Reason)
		return degraded.Message
	}
	message := degradedMessage()
	assert.Contains(t, message, "the configuration compresses to")
	t.Log(message)

	// A reconcile that checked the content again would replace the marker.
	reconciler.resultsMu.Lock()
	results := reconciler.results[nn]
	require.True(t, results.limitsChecked)
	results.limitsErr = "recorded limits result"
	reconciler.results[nn] = results
	reconciler.resultsMu.Unlock()
	assert.Contains(t, degradedMessage(), "recorded limits result")
}

// TestRuleSetReconciler_BoundsCompileErrors compiles a regular expression of
// 1.3 MiB that does not close: Coraza quotes it whole in its error, while the
// recorded result and the RuleSet status stay bounded.
func TestRuleSetReconciler_BoundsCompileErrors(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)
	var rules strings.Builder
	rules.WriteString(`SecRule ARGS "@rx (`)
	for line := 0; rules.Len() < 1300<<10; line++ {
		// Coraza reads lines of up to 64 KiB, so the expression continues
		// over many lines.
		rules.WriteString(strings.Repeat(fmt.Sprintf("w%07d", line), 4000) + "\\\n")
	}
	rules.WriteString(`" "id:1,phase:1,pass"`)
	source := utils.NewTestRuleSource("regex-src", ns, rules.String())
	source.Annotations = map[string]string{wafv1alpha1.AnnotationSkipValidation: "false"}
	require.NoError(t, k8sClient.Create(ctx, source))
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{Name: "regex-ruleset", Namespace: ns, Sources: []wafv1alpha1.SourceReference{{Name: source.Name}}})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	reconciler := &RuleSetReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: utils.NewFakeRecorder(),
		Cache:    cache.NewRuleSetCache(),
	}
	nn := client.ObjectKeyFromObject(ruleSet)
	_, err := reconcileRuleSetWithRuleSources(ctx, t, nn, reconciler)
	require.NoError(t, err)

	reconciler.resultsMu.Lock()
	results := reconciler.results[nn]
	reconciler.resultsMu.Unlock()
	require.True(t, results.compiled)
	assert.LessOrEqual(t, len(results.compileErr), maxResultMessageBytes)
	assert.Contains(t, results.compileErr, "w0000000")

	require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
	degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, conditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, ruleSetDegradedReasonInvalidRuleSet, degraded.Reason)
	assert.LessOrEqual(t, len(degraded.Message), maxResultMessageBytes+len("Ruleset is invalid\n"))
}

// TestPatchDegradedBoundsTheMessage writes a condition message far past the
// API's limit: it is cut, on a character boundary, so the patch succeeds.
func TestPatchDegradedBoundsTheMessage(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{Name: "long-message", Namespace: ns, Sources: []wafv1alpha1.SourceReference{{Name: "src"}}})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ruleSet)}

	message := strings.Repeat("é", 700<<10)
	require.NoError(t, patchDegraded(ctx, k8sClient.Status(), utils.NewTestRecorder(), logr.Discard(), req, "RuleSet", ruleSet, &ruleSet.Status.Conditions, ruleSet.Generation, "Long", message))

	var current wafv1alpha1.RuleSet
	require.NoError(t, k8sClient.Get(ctx, req.NamespacedName, &current))
	degraded := apimeta.FindStatusCondition(current.Status.Conditions, conditionDegraded)
	require.NotNil(t, degraded)
	assert.LessOrEqual(t, len(degraded.Message), maxConditionMessageBytes)
	assert.True(t, utf8.ValidString(degraded.Message))
	assert.True(t, strings.HasSuffix(degraded.Message, "é..."))
}

// TestPublicationCoalescedReconcileSkipsTheSizeCheck grows the inputs of a
// published Engine past the budget and records them as published: a reconcile
// that has nothing to publish returns before scanning them.
func TestPublicationCoalescedReconcileSkipsTheSizeCheck(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)
	createTestGateway(t, ctx, k8sClient, "coalesce-gw", ns)
	createPublicationRuleSource(t, ctx, ns, "coalesce-source", publicationRules)
	data := make([]*wafv1alpha1.RuleData, 8)
	refs := make([]wafv1alpha1.DataReference, len(data))
	for i := range data {
		data[i] = utils.NewTestRuleData(fmt.Sprintf("coalesce-data-%d", i), ns, map[string]string{fmt.Sprintf("f%d.data", i): "x"})
		require.NoError(t, k8sClient.Create(ctx, data[i]))
		refs[i].Name = data[i].Name
	}
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "coalesce",
		Namespace: ns,
		Sources:   []wafv1alpha1.SourceReference{{Name: "coalesce-source"}},
		Data:      refs,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	engine := createPublicationEngine(t, ctx, ns, "engine", ruleSet.Name, "coalesce-gw")
	inputs, err := loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)
	content, err := assemble.Assemble(inputs)
	require.NoError(t, err)
	accepted := acceptedConfigurationFor(t, engine, ruleSet, content, nil)
	writeAcceptedConfiguration(t, ctx, engine, accepted)

	publication := newPublicationReconciler(t)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(engine)}
	_, err = publication.Reconcile(ctx, request)
	require.NoError(t, err)

	for i, d := range data {
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(d), d))
		d.Spec.Files = map[string]string{fmt.Sprintf("f%d.data", i): strings.Repeat("a", 1<<20)}
		require.NoError(t, k8sClient.Update(ctx, d))
	}
	inputs, err = loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)
	require.ErrorIs(t, checkInputsSize(inputs), errContentTooLarge, "a size check would refuse these inputs")

	publication.remember(request.NamespacedName, assemble.Fingerprint(inputs), accepted.Version)
	require.NoError(t, publication.reconstruct(ctx, request.NamespacedName, engine, accepted))
}

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
	"cmp"
	"errors"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/types"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/validation"
)

// DefaultMaxRules is the default limit on the rules of one RuleSet; see
// validation.CountRules.
const DefaultMaxRules = 20000

// maxResultMessageBytes bounds a remembered check message: a Coraza error can
// quote a whole regular expression.
const maxResultMessageBytes = 4 << 10

// errContentTooLarge is wrapped for RuleSet content past a delivery limit or
// the rule limit.
var errContentTooLarge = errors.New("content exceeds a limit")

// checkInputsSize adds up the size the rule text and RuleData files of loaded
// inputs take in the delivered configuration, as it encodes them, and refuses
// content past render.ContentBudget, which no Engine can deliver. It is kept
// apart from loading, so a publication that has nothing to do does not scan
// the content.
func checkInputsSize(inputs assemble.Inputs) error {
	var size contentSize
	// RuleData merges last-listed-wins: count from the last listed, and skip
	// a file name already counted, so only the delivered value counts.
	for _, data := range slices.Backward(inputs.Data) {
		if err := size.addData(data.Name, data.Files); err != nil {
			return err
		}
	}
	for _, source := range inputs.Sources {
		if err := size.addSource(source.Name, source.Rules); err != nil {
			return err
		}
	}
	return nil
}

// contentSize counts inputs one by one, so a refusal names the input that
// crossed the budget.
type contentSize struct {
	encoded int
	sources int
	files   map[string]struct{}
	largest contribution
}

// contribution is the encoded size one input adds.
type contribution struct {
	kind, name string
	bytes      int
}

func (s *contentSize) addSource(name, rules string) error {
	added := render.EncodedLen(rules)
	if s.sources > 0 {
		added += 2 // sources are joined with a newline, which encodes as `\n`
	}
	s.sources++
	return s.add("RuleSource", name, added)
}

func (s *contentSize) addData(name string, files map[string]string) error {
	if s.files == nil {
		s.files = make(map[string]struct{})
	}
	added := 0
	for file, data := range files {
		if _, counted := s.files[file]; counted {
			continue
		}
		if len(s.files) > 0 {
			added++ // the comma between entries
		}
		s.files[file] = struct{}{}
		added += render.DataEntryLen(file, data)
	}
	return s.add("RuleData", name, added)
}

func (s *contentSize) add(kind, name string, added int) error {
	s.encoded += added
	if added > s.largest.bytes {
		s.largest = contribution{kind: kind, name: name, bytes: added}
	}
	if s.encoded <= render.ContentBudget {
		return nil
	}
	return fmt.Errorf("%w: rule text and RuleData encode to %d bytes at %s %s, above the %d bytes one configuration can carry; the largest input is %s %s with %d bytes",
		errContentTooLarge, s.encoded, kind, name, render.ContentBudget, s.largest.kind, s.largest.name, s.largest.bytes)
}

// checkContentLimits refuses assembled content an Engine would refuse, past
// its delivery limits or too large once compressed, and content with more
// rules than MaxRules. The result only depends on the content, so it is
// computed once per content.
func (r *RuleSetReconciler) checkContentLimits(key types.NamespacedName, contentHash string, content assemble.Content) error {
	results := r.contentResults(key, contentHash)
	if !results.limitsChecked {
		results.limitsChecked = true
		results.limitsErr = r.limitsError(content)
		r.storeContentResults(key, results)
	}
	if results.limitsErr != "" {
		return fmt.Errorf("%w: %s", errContentTooLarge, results.limitsErr)
	}
	return nil
}

func (r *RuleSetReconciler) limitsError(content assemble.Content) string {
	if err := validateContentLimits(content, r.Limits.WithDefaults()); err != nil {
		return err.Error()
	}
	maxRules := cmp.Or(r.MaxRules, DefaultMaxRules)
	if validation.CountRules(content.Rules, content.DataFiles, maxRules) > maxRules {
		return fmt.Sprintf("more than %d SecRule, SecAction and SecMarker directives, counting each Include of a file", maxRules)
	}
	envelope, err := render.EnvelopeLen(content)
	switch {
	case err != nil:
		return err.Error()
	case envelope > render.MaxConfigurationBytes:
		return fmt.Sprintf("the configuration compresses to %d bytes, above the plugin limit of %d bytes", envelope, render.MaxConfigurationBytes)
	}
	return ""
}

// contentResults are the checks of one RuleSet's last content that are costly
// to repeat on every event and resync. They hold bounded messages, never error
// values, which can quote the whole content.
type contentResults struct {
	contentHash   string
	limitsChecked bool
	limitsErr     string
	compiled      bool
	compileErr    string
}

// contentResults returns the results recorded for this RuleSet's content, or
// empty results when its content changed.
func (r *RuleSetReconciler) contentResults(key types.NamespacedName, contentHash string) contentResults {
	r.resultsMu.Lock()
	defer r.resultsMu.Unlock()
	if results, ok := r.results[key]; ok && results.contentHash == contentHash {
		return results
	}
	return contentResults{contentHash: contentHash}
}

func (r *RuleSetReconciler) storeContentResults(key types.NamespacedName, results contentResults) {
	r.resultsMu.Lock()
	defer r.resultsMu.Unlock()
	if r.results == nil {
		r.results = make(map[types.NamespacedName]contentResults)
	}
	r.results[key] = results
}

// forgetContentResults drops the results of a deleted RuleSet.
func (r *RuleSetReconciler) forgetContentResults(key types.NamespacedName) {
	r.resultsMu.Lock()
	defer r.resultsMu.Unlock()
	delete(r.results, key)
}

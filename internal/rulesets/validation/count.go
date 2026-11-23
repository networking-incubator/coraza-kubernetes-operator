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
	"path"
	"strings"
)

// maxIncludes is how many Include directives Coraza follows in one compile;
// it refuses the content at the next one.
const maxIncludes = 100

// CountRules returns how many rules Coraza adds when it compiles rule text and
// the RuleData files it Includes, as BuildWAFConfig assembles them with the
// CRS tuning stand-in rules: every SecRule (each rule of a chain), SecAction
// and SecMarker. Coraza parses an Included file again at every Include, so a
// file's rules count once per Include, up to Coraza's limit of Includes.
// Counting stops once the count passes limit.
func CountRules(rules string, dataFiles map[string][]byte, limit int) int {
	counter := ruleCounter{dataFiles: dataFiles, limit: limit}
	counter.count("", compiledDirectives(rules, dataFiles))
	return counter.rules
}

type ruleCounter struct {
	dataFiles map[string][]byte
	texts     map[string]string // Included files, converted once
	limit     int
	rules     int
	includes  int
}

func (c *ruleCounter) done() bool {
	return c.rules > c.limit || c.includes > maxIncludes
}

// count counts the rules of text, read from the file name ("" for the rule
// text itself), following its Includes as Coraza does.
func (c *ruleCounter) count(name, text string) {
	_, _ = walkDirectives(text, func(line string) bool {
		directive, _, _ := strings.Cut(line, " ")
		switch {
		case strings.EqualFold(directive, "SecRule"), strings.EqualFold(directive, "SecAction"), strings.EqualFold(directive, "SecMarker"):
			c.rules++
		case strings.EqualFold(directive, "Include"):
			c.includes++
			if target, ok := ruleDataIncludeName(line); ok && !c.done() {
				included := path.Join(path.Dir(name), target)
				if text, ok := c.text(included); ok {
					c.count(included, text)
				}
			}
		}
		return !c.done()
	})
}

// text returns an Included file as a string, converting it once however often
// it is Included.
func (c *ruleCounter) text(name string) (string, bool) {
	if text, ok := c.texts[name]; ok {
		return text, true
	}
	data, ok := c.dataFiles[name]
	if !ok {
		return "", false
	}
	if c.texts == nil {
		c.texts = make(map[string]string)
	}
	c.texts[name] = string(data)
	return c.texts[name], true
}

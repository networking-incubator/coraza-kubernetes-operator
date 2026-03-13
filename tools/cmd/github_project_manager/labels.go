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

import "strings"

// hasLabelPrefix returns true if any label starts with prefix.
func hasLabelPrefix(labels []string, prefix string) bool {
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// filterLabels returns entries matching the predicate.
func filterLabels(ss []string, fn func(string) bool) []string {
	var out []string
	for _, s := range ss {
		if fn(s) {
			out = append(out, s)
		}
	}
	return out
}

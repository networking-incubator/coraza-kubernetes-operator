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

// Package validation provides Coraza validation helpers for rule text.
package validation

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/corazawaf/coraza/v3"
)

// ExtractMissingFileBasename extracts the base filename from a missing-file
// error by walking the error chain for *fs.PathError with fs.ErrNotExist.
func ExtractMissingFileBasename(err error) (string, bool) {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && errors.Is(pathErr.Err, fs.ErrNotExist) {
		return filepath.Base(pathErr.Path), true
	}
	return "", false
}

// SanitizeErrorMessage replaces filesystem paths in missing-file errors with
// just the base filename to prevent disclosure of container-internal paths.
func SanitizeErrorMessage(err error) error {
	if basename, ok := ExtractMissingFileBasename(err); ok {
		return fmt.Errorf("open %s: data does not exist", basename)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("validation failed: referenced file does not exist (path redacted)")
	}
	return err
}

// ShouldSkipMissingFileError reports whether a missing-file validation error
// should be skipped. When dataFiles is nil (RuleSource reconciler path, no
// RuleSet-scoped RuleData merged), any missing-file error is skipped so
// aggregate RuleSet validation remains authoritative for @pmFromFile. When
// dataFiles is non-nil, the error is skipped only if basename exists in the
// map (use an empty non-nil map to require resolution without providing files).
func ShouldSkipMissingFileError(err error, dataFiles map[string][]byte) bool {
	basename, ok := ExtractMissingFileBasename(err)
	if !ok {
		return false
	}
	if dataFiles == nil {
		return true
	}
	_, exists := dataFiles[basename]
	return exists
}

// ValidateRuleSourceRules validates SecLang rule text via Coraza. dataFiles
// supplies in-memory files for @pmFromFile resolution. nil means no files are
// provided: missing-file errors are skipped (RuleSource controller path).
// Pass a non-nil map (including an empty map) to require that missing files
// appear in the map. RuleSet aggregate validation remains authoritative for
// cross-source and full RuleData-backed rules.
func ValidateRuleSourceRules(rules, ruleSourceName string, dataFiles map[string][]byte) error {
	if IsPatchOnlyFragment(rules) {
		return nil
	}
	conf := coraza.NewWAFConfig().WithDirectives(rules)
	if _, err := coraza.NewWAF(conf); err != nil {
		if ShouldSkipMissingFileError(err, dataFiles) {
			return nil
		}
		return fmt.Errorf("RuleSource %s doesn't contain valid rules: %w", ruleSourceName, SanitizeErrorMessage(err))
	}
	return nil
}

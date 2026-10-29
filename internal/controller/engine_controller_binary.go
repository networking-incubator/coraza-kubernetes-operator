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
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
)

var binarySHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// resolveBinaryRef returns the raw WASM binary location and digest for an
// Engine. An explicit spec.driver.wasm.url/sha256 pair wins; omitting both
// selects the module bundled with the operator.
//
// shortcut: full SSRF/host-class validation and custom WAsm CA are Phase 5; add them here.
func (r *EngineReconciler) resolveBinaryRef(engine *wafv1alpha1.Engine) (render.BinaryRef, error) {
	var explicitURL, explicitSHA string
	if engine.Spec.Driver.Wasm != nil {
		explicitURL = engine.Spec.Driver.Wasm.URL
		explicitSHA = engine.Spec.Driver.Wasm.SHA256
	}

	switch {
	case explicitURL != "" && explicitSHA != "":
		ref := render.BinaryRef{URL: explicitURL, SHA256: explicitSHA}
		if err := validateBinaryRefBasic(ref); err != nil {
			return render.BinaryRef{}, err
		}
		return ref, nil

	case explicitURL == "" && explicitSHA == "":
		manifest, _, err := assets.ResolveDefault()
		if err != nil {
			return render.BinaryRef{}, fmt.Errorf("resolve bundled WASM module: %w", err)
		}
		ref := render.BinaryRef{
			URL:    strings.TrimRight(r.assetBaseURL, "/") + "/wasm/" + manifest.SHA256 + ".wasm",
			SHA256: manifest.SHA256,
		}
		if err := validateBinaryRefBasic(ref); err != nil {
			return render.BinaryRef{}, fmt.Errorf("resolve bundled WASM module: %w", err)
		}
		return ref, nil

	default:
		return render.BinaryRef{}, errors.New("spec.driver.wasm.url and spec.driver.wasm.sha256 must be set together; omit both to use the bundled module")
	}
}

// validateBinaryRefBasic validates the URL shape and digest of a binary
// reference. The CEL rules already reject these shapes for new writes; this is
// the runtime guard for already-stored objects.
func validateBinaryRefBasic(ref render.BinaryRef) error {
	parsed, err := url.Parse(ref.URL)
	if err != nil {
		return fmt.Errorf("parse WASM binary URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("WASM binary URL %q must be an absolute https URL with a host", ref.URL)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("WASM binary URL %q must not contain userinfo, query, or fragment", ref.URL)
	}
	if !binarySHA256Pattern.MatchString(ref.SHA256) {
		return fmt.Errorf("WASM sha256 %q must be 64 lowercase hex characters", ref.SHA256)
	}
	return nil
}

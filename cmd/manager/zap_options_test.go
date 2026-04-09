/*
Copyright Coraza Kubernetes Operator contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR ANY CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"testing"

	crlogzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func TestZapOptions_NoArgs_NotDevelopmentMode(t *testing.T) {
	t.Parallel()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	opts := crlogzap.Options{Development: false}
	opts.BindFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if opts.Development {
		t.Error("Development: want false when no CLI args (matches production default before addDefaults)")
	}
}

func TestZapOptions_ZapDevelTrue_EnablesDevelopment(t *testing.T) {
	t.Parallel()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	opts := crlogzap.Options{Development: false}
	opts.BindFlags(fs)
	if err := fs.Parse([]string{"--zap-devel=true"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !opts.Development {
		t.Error("Development: want true when --zap-devel=true")
	}
}

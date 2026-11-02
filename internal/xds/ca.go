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

package xds

import (
	"crypto/x509"
	"errors"
	"os"
	"sync"

	"github.com/go-logr/logr"
)

// CASource loads the xDS CA bundle on demand and caches the first valid read.
// A CA mounted after startup therefore becomes available to both the listener
// readiness check and the Engine reconciler without restarting the operator.
// Rotation still needs a restart: the first valid bundle stays cached.
type CASource struct {
	path   string
	logger logr.Logger

	mu  sync.Mutex
	pem []byte
}

// NewCASource returns a source that reads path on first use.
func NewCASource(path string, logger logr.Logger) *CASource {
	if logger.GetSink() == nil {
		logger = logr.Discard()
	}
	return &CASource{path: path, logger: logger}
}

// PEM returns the CA bundle, reading the file when it is not cached yet. It
// returns nil while the file is missing or holds no certificate.
func (c *CASource) PEM() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pem) > 0 {
		return c.pem
	}
	caPEM, err := os.ReadFile(c.path)
	if err != nil {
		return nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil
	}
	c.pem = caPEM
	c.logger.Info("Loaded the xDS CA bundle", "path", c.path)
	return c.pem
}

// Ready reports whether a valid CA bundle has been loaded.
func (c *CASource) Ready() error {
	if len(c.PEM()) == 0 {
		return errors.New("xDS CA bundle is empty")
	}
	return nil
}

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
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

const (
	// certificateExpiryWarning is how long before expiry the operator starts
	// warning about the serving certificate or the CA bundle.
	certificateExpiryWarning = 30 * 24 * time.Hour
	// certificateExpiryInterval is how often the warning is repeated.
	certificateExpiryInterval = 24 * time.Hour
)

var errCertificateExpired = errors.New("certificate expired")

// certificateExpiry exports when the serving certificate and the CA chain the
// operator distributes expire, and logs a warning when either expires within
// 30 days and an error once it has expired. Gateways reject an expired chain,
// so this is the signal to rotate; readiness deliberately ignores it.
type certificateExpiry struct {
	metrics *Metrics
	logger  logr.Logger
	now     func() time.Time

	mu        sync.Mutex
	leaf      *tls.Certificate
	bundle    []*x509.Certificate
	servingAt time.Time
	caAt      time.Time
}

func newCertificateExpiry(metrics *Metrics, logger logr.Logger) *certificateExpiry {
	return &certificateExpiry{metrics: metrics, logger: logger, now: time.Now}
}

// observeServingCert records the serving certificate current returns. Reloads
// are reported from separate goroutines in any order, so it reads the current
// certificate under the lock rather than taking the one a reload passed.
func (e *certificateExpiry) observeServingCert(current func(*tls.ClientHelloInfo) (*tls.Certificate, error)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cert, _ := current(nil); cert != nil && cert.Leaf != nil {
		e.leaf = cert
		e.updateLocked()
	}
}

// observeCA records a loaded CA bundle.
func (e *certificateExpiry) observeCA(bundle []byte) {
	var certs []*x509.Certificate
	for block, rest := pem.Decode(bundle); block != nil; block, rest = pem.Decode(rest) {
		if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
			certs = append(certs, cert)
		}
	}
	if len(certs) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bundle = certs
	e.updateLocked()
}

// refresh recomputes and reports both expiries: the chain that verifies the
// serving certificate can change as certificates expire.
func (e *certificateExpiry) refresh() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.servingAt, e.caAt = time.Time{}, time.Time{}
	e.updateLocked()
}

// updateLocked sets the gauges and reports each expiry that changed. The CA
// expiry depends on the chain the serving certificate uses, so it is reported
// once that certificate is known.
func (e *certificateExpiry) updateLocked() {
	if e.leaf == nil {
		return
	}
	if servingAt := e.leaf.Leaf.NotAfter; !servingAt.Equal(e.servingAt) {
		e.servingAt = servingAt
		e.metrics.SetServingCertNotAfter(servingAt)
		e.report("serving certificate", servingAt)
	}
	if len(e.bundle) == 0 {
		return
	}
	caAt, err := e.chainNotAfter()
	if err != nil {
		e.logger.Error(err, "No certificate chain in the xDS CA bundle verifies the serving certificate; reporting the earliest CA expiry")
	}
	if !caAt.Equal(e.caAt) {
		e.caAt = caAt
		e.metrics.SetCANotAfter(caAt)
		e.report("CA bundle", caAt)
	}
}

// chainNotAfter returns when the CA chain that verifies the serving
// certificate expires: the earliest expiry among its intermediates and root,
// on the chain that stays valid the longest. A bundle can keep an expired CA
// next to the one that signs the current certificate. When no chain verifies
// it, it returns the earliest expiry among the bundle and the intermediates in
// tls.crt, and why.
func (e *certificateExpiry) chainNotAfter() (time.Time, error) {
	roots := x509.NewCertPool()
	for _, cert := range e.bundle {
		roots.AddCert(cert)
	}
	intermediates := x509.NewCertPool()
	candidates := slices.Clone(e.bundle)
	for _, der := range e.leaf.Certificate[min(1, len(e.leaf.Certificate)):] {
		if cert, err := x509.ParseCertificate(der); err == nil {
			intermediates.AddCert(cert)
			candidates = append(candidates, cert)
		}
	}
	// Verify at a time the leaf is valid, so an expired leaf, or one not
	// valid yet, still finds the chain it was issued under.
	leaf := e.leaf.Leaf
	at := e.now()
	if at.Before(leaf.NotBefore) {
		at = leaf.NotBefore
	}
	if at.After(leaf.NotAfter) {
		at = leaf.NotAfter
	}
	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   at,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	if err != nil {
		// An expired intermediate from tls.crt fails the chain as surely as
		// an expired CA in the bundle.
		return earliestNotAfter(candidates), err
	}
	var latest time.Time
	for _, chain := range chains {
		authorities := chain[1:]
		if len(authorities) == 0 {
			authorities = chain // the leaf is itself a trusted root
		}
		if notAfter := earliestNotAfter(authorities); notAfter.After(latest) {
			latest = notAfter
		}
	}
	return latest, nil
}

func earliestNotAfter(certs []*x509.Certificate) time.Time {
	return slices.MinFunc(certs, func(a, b *x509.Certificate) int { return a.NotAfter.Compare(b.NotAfter) }).NotAfter
}

// run refreshes the expiries once a day until ctx ends.
func (e *certificateExpiry) run(ctx context.Context) {
	ticker := time.NewTicker(certificateExpiryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.refresh()
		}
	}
}

func (e *certificateExpiry) report(certificate string, notAfter time.Time) {
	if notAfter.IsZero() {
		return
	}
	switch remaining := notAfter.Sub(e.now()); {
	case remaining <= 0:
		e.logger.Error(errCertificateExpired, "The xDS certificate has expired; Gateways cannot verify the operator until it is rotated",
			"certificate", certificate, "notAfter", notAfter)
	case remaining < certificateExpiryWarning:
		e.logger.Info("The xDS certificate expires soon; rotate it before it expires",
			"certificate", certificate, "notAfter", notAfter, "remaining", remaining.Round(time.Hour).String())
	}
}

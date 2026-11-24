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
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// recordsPerStream sizes the delivery records for the streams a replica admits:
// a proxy keeps its record after its streams end, and reconnects overlap.
const recordsPerStream = 4

// proxyCacheKey identifies one proxy of an Engine: its identity and the node ID
// it reports. The node ID partitions the proxies of a ServiceAccount; it never
// authorizes an Engine.
func proxyCacheKey(engineKey string, identity Identity, proxyID string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s", engineKey, identity.Key(), identity.UID, proxyID)))
	return hex.EncodeToString(sum[:])
}

// delivered is a configuration a proxy was sent: a content version under a tag.
type delivered struct {
	version string
	tag     tag
}

// delivery is what this process knows of one proxy's configuration: what it last
// sent, whether the proxy acknowledged that tag, and how many times in a row the
// proxy has rejected a configuration of that content since it last acknowledged
// one.
type delivery struct {
	key       string
	engineKey string
	sent      delivered
	acked     bool
	attempts  int
}

// deliveries holds the delivery records of the proxies, at most max of them.
// A record is written when a response is sent, before the proxy can answer it,
// so a rejection that never reaches this process still leaves a record: the
// proxy is not sent the tag again. Only a response writes one: a stream that
// sends nothing but rejections, under any number of node IDs, evicts none. Losing
// a record restarts the proxy's back-off, and when the record held the Engine's
// current tag unacknowledged, the Engine moves to its next generation.
type deliveries struct {
	store *SnapshotStore
	max   int

	mu    sync.Mutex
	byKey map[string]*list.Element // of *delivery
	order *list.List               // the most recently written first
}

func newDeliveries(store *SnapshotStore, maxRecords int) *deliveries {
	return &deliveries{store: store, max: max(1, maxRecords), byKey: make(map[string]*list.Element), order: list.New()}
}

// touchLocked returns the proxy's record, moved to the front, or nil.
func (d *deliveries) touchLocked(key string) *delivery {
	element, ok := d.byKey[key]
	if !ok {
		return nil
	}
	d.order.MoveToFront(element)
	return element.Value.(*delivery)
}

// writeLocked returns the proxy's record, new or moved to the front, and drops
// the oldest record beyond the bound. A dropped record that holds its Engine's
// current tag unacknowledged advances the Engine, so that no proxy without a
// record is sent a tag some proxy may have rejected. Dropping and advancing are
// one step under the lock: whoever finds the record gone finds the Engine moved on.
func (d *deliveries) writeLocked(key, engineKey string) *delivery {
	if record := d.touchLocked(key); record != nil {
		return record
	}
	record := &delivery{key: key, engineKey: engineKey}
	d.byKey[key] = d.order.PushFront(record)
	if d.order.Len() > d.max {
		oldest := d.order.Back()
		dropped := oldest.Value.(*delivery)
		d.order.Remove(oldest)
		delete(d.byKey, dropped.key)
		if !dropped.acked {
			d.store.Advance(dropped.engineKey, dropped.sent.tag)
		}
	}
	return record
}

// sent records that the proxy was sent a content version under a tag. Other
// content than it was sent before starts its back-off again: the back-off is for
// a configuration that stays rejected, not for one the Engine has since replaced.
func (d *deliveries) sent(key, engineKey, version string, t tag) {
	d.mu.Lock()
	defer d.mu.Unlock()
	record := d.writeLocked(key, engineKey)
	if record.sent.version != version {
		record.attempts = 0
	}
	record.sent, record.acked = delivered{version: version, tag: t}, false
}

// acknowledged records that the proxy accepted what it was last sent, and ends
// its back-off. An acknowledgement of anything else, as a late one of an earlier
// stream is, is not one of it.
func (d *deliveries) acknowledged(key string, accepted delivered) {
	d.mu.Lock()
	defer d.mu.Unlock()
	element, ok := d.byKey[key]
	if !ok {
		return
	}
	record := element.Value.(*delivery)
	if record.sent == accepted {
		record.acked, record.attempts = true, 0
	}
}

// rejected records a rejection by the proxy of the configuration named, or, when
// it names none, as a rejection replayed on a new stream does not, of what the
// proxy was last sent. It returns how many times in a row the proxy has rejected
// a configuration of that content, and the configuration rejected. A proxy that
// was sent nothing has no record, and is on its first rejection, and so is one
// that rejects content it was sent before what it was last sent: that does not
// count against the back-off of the content it has now.
func (d *deliveries) rejected(key string, named delivered) (attempts int, rejected delivered) {
	d.mu.Lock()
	defer d.mu.Unlock()
	record := d.touchLocked(key)
	switch {
	case record == nil:
		return 1, named
	case named.version == "":
		named = record.sent
	case named.version != record.sent.version:
		return 1, named
	}
	record.attempts++
	return record.attempts, named
}

// unacknowledged reports whether the proxy was sent t and has not acknowledged it.
func (d *deliveries) unacknowledged(key string, t tag) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	element, ok := d.byKey[key]
	if !ok {
		return false
	}
	record := element.Value.(*delivery)
	return record.sent.tag == t && !record.acked
}

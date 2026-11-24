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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeliveriesTrackWhatTheProxyWasSentAndAcknowledged(t *testing.T) {
	d := newDeliveries(NewSnapshotStore(), 8)
	first, second := tag{epoch: otherProcessEpoch}, tag{epoch: otherProcessEpoch, gen: 1}
	assert.False(t, d.unacknowledged("proxy", first), "nothing was sent")

	d.sent("proxy", storeEngineKey, "sha256:v1", first)
	assert.True(t, d.unacknowledged("proxy", first))
	assert.False(t, d.unacknowledged("proxy", second))
	d.acknowledged("proxy", delivered{version: "sha256:v1", tag: second})
	assert.True(t, d.unacknowledged("proxy", first), "an acknowledgement of another tag is not one of this tag")

	attempts, last := d.rejected("proxy", delivered{})
	assert.Equal(t, 1, attempts)
	assert.Equal(t, delivered{version: "sha256:v1", tag: first}, last, "a rejection that names nothing is of what was last sent")
	attempts, _ = d.rejected("proxy", delivered{})
	assert.Equal(t, 2, attempts)
	named := delivered{version: "sha256:v1", tag: second}
	attempts, last = d.rejected("proxy", named)
	assert.Equal(t, 3, attempts, "rejections of the same content count across tags")
	assert.Equal(t, named, last, "a rejection that names a configuration is of it")

	d.acknowledged("proxy", delivered{version: "sha256:v1", tag: first})
	assert.False(t, d.unacknowledged("proxy", first))
	attempts, _ = d.rejected("proxy", delivered{})
	assert.Equal(t, 1, attempts, "an acknowledgement ends the back-off")

	d.sent("proxy", storeEngineKey, "sha256:v2", second)
	assert.True(t, d.unacknowledged("proxy", second))
	assert.False(t, d.unacknowledged("proxy", first))
	_, last = d.rejected("proxy", delivered{})
	assert.Equal(t, delivered{version: "sha256:v2", tag: second}, last)
}

// The back-off is for a configuration that stays rejected. A proxy that is sent
// other content than before starts its ladder again, however long it was, and
// escalates again only for rejections of the same content, whichever tag they
// were sent under.
func TestDeliveriesBackoffRestartsForOtherContent(t *testing.T) {
	d := newDeliveries(NewSnapshotStore(), 8)
	d.sent("proxy", storeEngineKey, "sha256:v1", tag{epoch: otherProcessEpoch})
	for want := 1; want <= 12; want++ {
		attempts, _ := d.rejected("proxy", delivered{})
		require.Equal(t, want, attempts)
		d.sent("proxy", storeEngineKey, "sha256:v1", tag{epoch: otherProcessEpoch, gen: uint64(want)})
	}

	d.sent("proxy", storeEngineKey, "sha256:v2", tag{epoch: otherProcessEpoch, gen: 13})
	attempts, _ := d.rejected("proxy", delivered{})
	assert.Equal(t, 1, attempts, "other content starts the ladder again")
	d.sent("proxy", storeEngineKey, "sha256:v2", tag{epoch: otherProcessEpoch, gen: 14})
	attempts, _ = d.rejected("proxy", delivered{})
	assert.Equal(t, 2, attempts, "and escalates for the same content")

	// An acknowledgement ends the ladder, whatever content is sent next.
	d.acknowledged("proxy", delivered{version: "sha256:v2", tag: tag{epoch: otherProcessEpoch, gen: 14}})
	attempts, _ = d.rejected("proxy", delivered{})
	assert.Equal(t, 1, attempts)
}

// An acknowledgement is of one configuration: the content under the tag it was
// sent. A late one of an earlier stream, of the content the proxy was sent before
// under the same tag, is not one of what it was sent since, which it may yet reject.
func TestDeliveriesAcknowledgementIsOfTheContentAndTheTag(t *testing.T) {
	d := newDeliveries(NewSnapshotStore(), 8)
	same := tag{epoch: otherProcessEpoch}
	d.sent("proxy", storeEngineKey, "sha256:v1", same)
	d.sent("proxy", storeEngineKey, "sha256:v2", same)

	d.acknowledged("proxy", delivered{version: "sha256:v1", tag: same})
	assert.True(t, d.unacknowledged("proxy", same), "the late acknowledgement of the first content")

	d.rejected("proxy", delivered{})
	d.acknowledged("proxy", delivered{version: "sha256:v2", tag: same})
	assert.False(t, d.unacknowledged("proxy", same), "the acknowledgement of what it was last sent")
	attempts, _ := d.rejected("proxy", delivered{})
	assert.Equal(t, 1, attempts, "ends its back-off")
}

// A rejection of content the proxy was sent before what it was last sent is
// rejected once, and does not count against the back-off of the content it has now:
// the first rejection of that starts at the first step.
func TestDeliveriesRejectionOfEarlierContentDoesNotCountAgainstTheCurrent(t *testing.T) {
	d := newDeliveries(NewSnapshotStore(), 8)
	d.sent("proxy", storeEngineKey, "sha256:v1", tag{epoch: otherProcessEpoch})
	d.sent("proxy", storeEngineKey, "sha256:v2", tag{epoch: otherProcessEpoch, gen: 1})

	attempts, rejected := d.rejected("proxy", delivered{version: "sha256:v1", tag: tag{epoch: otherProcessEpoch}})
	assert.Equal(t, 1, attempts)
	assert.Equal(t, delivered{version: "sha256:v1", tag: tag{epoch: otherProcessEpoch}}, rejected, "it is the earlier content that was rejected")
	attempts, _ = d.rejected("proxy", delivered{})
	assert.Equal(t, 1, attempts, "the first rejection of the content it has now")

	// Rejections of the same content under other tags keep counting.
	attempts, _ = d.rejected("proxy", delivered{version: "sha256:v2", tag: tag{epoch: otherProcessEpoch, gen: 7}})
	assert.Equal(t, 2, attempts)
}

// A rejection writes no record: only a response does. A proxy that was sent
// nothing is on its first rejection, and streams that send nothing but
// rejections, under node IDs of their own, evict no other proxy's record.
func TestDeliveriesRejectionsWriteNoRecords(t *testing.T) {
	store := publishedStore(t)
	const bound = 4
	d := newDeliveries(store, bound)
	held := tag{epoch: store.epoch}
	d.sent("victim", storeEngineKey, "sha256:v1", held)

	for i := range 10 * bound {
		attempts, last := d.rejected(fmt.Sprintf("churn-%d", i), delivered{})
		assert.Equal(t, 1, attempts)
		assert.Equal(t, delivered{}, last, "a proxy that was sent nothing has no last configuration")
	}

	assert.True(t, d.unacknowledged("victim", held), "the record is kept")
	d.mu.Lock()
	assert.Equal(t, 1, d.order.Len())
	d.mu.Unlock()
	store.mu.RLock()
	defer store.mu.RUnlock()
	assert.Zero(t, store.engines[storeEngineKey].gen, "no record was dropped, so the Engine did not move on")
}

// A rejection keeps its proxy's record from being the next to drop.
func TestDeliveriesRejectionKeepsTheRecordFresh(t *testing.T) {
	const bound = 3
	d := newDeliveries(publishedStore(t), bound)
	d.sent("rejecting", storeEngineKey, "sha256:v1", tag{epoch: otherProcessEpoch})
	for i := range bound - 1 {
		d.sent(fmt.Sprintf("newer-%d", i), storeEngineKey, "sha256:v1", tag{epoch: otherProcessEpoch, gen: uint64(i + 1)})
	}
	attempts, _ := d.rejected("rejecting", delivered{})
	assert.Equal(t, 1, attempts)

	d.sent("newest", storeEngineKey, "sha256:v1", tag{epoch: otherProcessEpoch, gen: 9})
	assert.True(t, d.unacknowledged("rejecting", tag{epoch: otherProcessEpoch}), "the record of the proxy that answered last stays")
	assert.False(t, d.unacknowledged("newer-0", tag{epoch: otherProcessEpoch, gen: 1}), "the oldest record was dropped")
}

// A record dropped for room advances its Engine when it holds the Engine's
// current tag unacknowledged: no proxy that has no record is then sent a tag
// the proxy may have rejected.
func TestDeliveriesDropTheOldestRecord(t *testing.T) {
	tests := []struct {
		name    string
		tag     func(*SnapshotStore) tag
		acked   bool
		wantGen uint64
	}{
		{name: "an unacknowledged current tag advances the Engine", tag: func(s *SnapshotStore) tag { return tag{epoch: s.epoch} }, wantGen: 1},
		{name: "an acknowledged tag does not", tag: func(s *SnapshotStore) tag { return tag{epoch: s.epoch} }, acked: true},
		{name: "a tag that is not the current one does not", tag: func(*SnapshotStore) tag { return tag{epoch: otherProcessEpoch} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := publishedStore(t)
			const bound = 4
			d := newDeliveries(store, bound)
			held := tt.tag(store)
			d.sent("oldest", storeEngineKey, "sha256:v1", held)
			if tt.acked {
				d.acknowledged("oldest", delivered{version: "sha256:v1", tag: held})
			}
			for i := range bound {
				d.sent(fmt.Sprintf("newer-%d", i), storeEngineKey, "sha256:v1", tag{epoch: otherProcessEpoch, gen: uint64(i + 1)})
			}

			assert.False(t, d.unacknowledged("oldest", held), "the oldest record was dropped")
			store.mu.RLock()
			defer store.mu.RUnlock()
			assert.Equal(t, tt.wantGen, store.engines[storeEngineKey].gen)
		})
	}
}

func TestDeliveriesStayWithinTheirBound(t *testing.T) {
	const bound = 16
	d := newDeliveries(publishedStore(t), bound)
	for i := range 10 * bound {
		d.sent(fmt.Sprintf("proxy-%d", i), storeEngineKey, "sha256:v1", tag{epoch: otherProcessEpoch, gen: uint64(i)})
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	require.Equal(t, bound, d.order.Len())
	require.Len(t, d.byKey, bound)
	_, kept := d.byKey[fmt.Sprintf("proxy-%d", 10*bound-1)]
	assert.True(t, kept, "the newest record is kept")
}

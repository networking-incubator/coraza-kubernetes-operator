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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func TestSnapshotStoreReplaceNotifiesOnlyOnVersionChange(t *testing.T) {
	store := NewSnapshotStore()
	changed := store.Changed()

	first := &Snapshot{ResourceName: "r1", Namespace: "shop", EngineName: "checkout", Version: "v1"}
	if !store.Replace(first) {
		t.Fatal("Replace(new) = false, want true")
	}
	select {
	case <-changed:
	default:
		t.Fatal("Changed() was not closed after the first Replace")
	}

	changed = store.Changed()
	if store.Replace(&Snapshot{ResourceName: "r1", Version: "v1"}) {
		t.Fatal("Replace(same version) = true, want false")
	}
	select {
	case <-changed:
		t.Fatal("Changed() was closed for an unchanged version")
	case <-time.After(50 * time.Millisecond):
	}

	if store.Replace(&Snapshot{ResourceName: "r1", Version: "v2"}) != true {
		t.Fatal("Replace(new version) = false, want true")
	}
	select {
	case <-changed:
	default:
		t.Fatal("Changed() was not closed for a new version")
	}

	snapshot, ok := store.Get("r1")
	if !ok || snapshot.Version != "v2" {
		t.Fatalf("Get() = %+v, %v; want v2", snapshot, ok)
	}
}

func TestSnapshotStoreDeleteAndDeleteEngine(t *testing.T) {
	store := NewSnapshotStore()
	store.Replace(&Snapshot{ResourceName: "a", Namespace: "shop", EngineName: "one", Version: "v1"})
	store.Replace(&Snapshot{ResourceName: "b", Namespace: "shop", EngineName: "one", Version: "v1"})
	store.Replace(&Snapshot{ResourceName: "c", Namespace: "shop", EngineName: "two", Version: "v1"})

	store.DeleteEngine("shop", "one")
	for _, name := range []string{"a", "b"} {
		if _, ok := store.Get(name); ok {
			t.Fatalf("snapshot %q still present after DeleteEngine", name)
		}
	}
	if _, ok := store.Get("c"); !ok {
		t.Fatal("unrelated snapshot was deleted")
	}

	if !store.Delete("c") {
		t.Fatal("Delete(existing) = false")
	}
	if store.Delete("c") {
		t.Fatal("Delete(missing) = true")
	}
}

func TestSnapshotStoreChangedIsPerStore(t *testing.T) {
	store := NewSnapshotStore()
	before := store.Changed()
	store.Replace(&Snapshot{ResourceName: "r", Version: "v"})
	after := store.Changed()
	if before == after {
		t.Fatal("Changed() returned the same channel after a broadcast")
	}
}

func TestSnapshotStoreWaitReturnsVersionChange(t *testing.T) {
	store := NewSnapshotStore()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan *Snapshot, 1)
	go func() {
		snapshot, err := store.Wait(ctx, "r", "v1")
		if err != nil {
			return
		}
		done <- snapshot
	}()

	store.Replace(&Snapshot{ResourceName: "r", Version: "v1"})
	select {
	case snapshot := <-done:
		t.Fatalf("Wait() returned the current version %q", snapshot.Version)
	case <-time.After(100 * time.Millisecond):
	}

	store.Replace(&Snapshot{ResourceName: "r", Version: "v2"})
	select {
	case snapshot := <-done:
		if snapshot.Version != "v2" {
			t.Fatalf("Wait() version = %q, want v2", snapshot.Version)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait() did not return after a version change")
	}
}

func TestSnapshotStoreWaitHonorsContext(t *testing.T) {
	store := NewSnapshotStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Wait(ctx, "r", ""); err == nil {
		t.Fatal("Wait() error = nil for a cancelled context")
	}
}

func TestResourceNameMatchesRenderContract(t *testing.T) {
	got := ResourceName("shop", "checkout", types.UID("11111111-1111-4111-8111-111111111111"))
	want := "coraza.waf.shop.checkout.11111111-1111-4111-8111-111111111111"
	if got != want {
		t.Fatalf("ResourceName() = %q, want %q", got, want)
	}
}

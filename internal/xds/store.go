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
	"sync"
)

// SnapshotStore holds the latest published snapshot per ECDS resource name.
// Replace is atomic and StoreChanged broadcasts to every waiter through a
// closed-and-replaced channel, so a slow stream never blocks a writer.
type SnapshotStore struct {
	mu        sync.RWMutex
	snapshots map[string]*Snapshot
	changed   chan struct{}
}

// NewSnapshotStore returns an empty store.
func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{
		snapshots: make(map[string]*Snapshot),
		changed:   make(chan struct{}),
	}
}

// Replace stores snapshot under its resource name and reports whether the
// effective version changed. A new resource always counts as a change.
func (s *SnapshotStore) Replace(snapshot *Snapshot) bool {
	if snapshot == nil || snapshot.ResourceName == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	previous, existed := s.snapshots[snapshot.ResourceName]
	s.snapshots[snapshot.ResourceName] = snapshot
	changed := !existed || previous.Version != snapshot.Version
	if changed {
		s.broadcastLocked()
	}
	return changed
}

// Get returns the current snapshot for resourceName.
func (s *SnapshotStore) Get(resourceName string) (*Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot, ok := s.snapshots[resourceName]
	return snapshot, ok
}

// Delete removes a resource snapshot and reports whether it existed.
func (s *SnapshotStore) Delete(resourceName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.snapshots[resourceName]; !ok {
		return false
	}
	delete(s.snapshots, resourceName)
	s.broadcastLocked()
	return true
}

// DeleteEngine removes every snapshot belonging to an Engine, including a
// snapshot left under a stale Engine UID after recreation.
func (s *SnapshotStore) DeleteEngine(namespace, engineName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for name, snapshot := range s.snapshots {
		if snapshot.Namespace == namespace && snapshot.EngineName == engineName {
			delete(s.snapshots, name)
			changed = true
		}
	}
	if changed {
		s.broadcastLocked()
	}
}

// Wait blocks until the snapshot for resourceName differs from afterVersion
// (or first appears) and returns it. It returns ctx.Err() when ctx ends.
func (s *SnapshotStore) Wait(ctx context.Context, resourceName, afterVersion string) (*Snapshot, error) {
	for {
		changed := s.Changed()
		if snapshot, ok := s.Get(resourceName); ok && snapshot.Version != afterVersion {
			return snapshot, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

// Changed returns a channel that is closed on the next store change.
func (s *SnapshotStore) Changed() <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.changed
}

func (s *SnapshotStore) broadcastLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

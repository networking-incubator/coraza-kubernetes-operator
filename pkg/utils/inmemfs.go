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

package utils

import (
	"bytes"
	"io/fs"
	"sync"
	"time"
)

// MemFS is a simple, thread-safe in-memory filesystem
type MemFS struct {
	mu    sync.RWMutex
	files map[string][]byte
}

// NewMemFS creates a new instance of an in-memory filesystem that implements fs.FS
func NewMemFS() *MemFS {
	return &MemFS{files: make(map[string][]byte)}
}

// WriteFile adds or updates a file in memory
func (m *MemFS) WriteFile(name string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	copied := make([]byte, len(data))
	copy(copied, data)
	m.files[name] = copied
}

// Open implements the fs.FS interface (Read-Only access)
func (m *MemFS) Open(name string) (fs.File, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	data, ok := m.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	return &memFile{
		name:   name,
		Reader: bytes.NewReader(data),
		size:   int64(len(data)),
	}, nil
}

// memFile wraps bytes.Reader to satisfy the fs.File interface
type memFile struct {
	name string
	*bytes.Reader
	size int64
}

// Stat returns a FileInfo from a file
func (f *memFile) Stat() (fs.FileInfo, error) { return f, nil }

// Close closes the reader of a file
func (f *memFile) Close() error { return nil }

// Name returns a file name
func (f *memFile) Name() string { return f.name }

// Size returns the size of a file content
func (f *memFile) Size() int64 { return f.size }

// Mode returns a file access mode
func (f *memFile) Mode() fs.FileMode { return 0644 }

// ModTime returns a file modification time
func (f *memFile) ModTime() time.Time { return time.Now() }

// IsDir returns a boolean that represents if a file entry is a directory
func (f *memFile) IsDir() bool { return false }

// Sys does not return anything
func (f *memFile) Sys() any { return nil }

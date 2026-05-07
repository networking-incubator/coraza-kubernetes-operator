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

package memfs_test

import (
	"io"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/memfs"
)

func TestOpenNonExistentFile(t *testing.T) {
	mfs := memfs.NewMemFS()

	file, err := mfs.Open("does-not-exist")
	assert.Nil(t, file)
	assert.ErrorIs(t, err, fs.ErrNotExist)

	var pathErr *fs.PathError
	require.ErrorAs(t, err, &pathErr)
	assert.Equal(t, "open", pathErr.Op)
	assert.Equal(t, "does-not-exist", pathErr.Path)
}

func TestWriteAndReadFile(t *testing.T) {
	mfs := memfs.NewMemFS()

	mfs.WriteFile("hello.txt", []byte("hello world"))

	file, err := mfs.Open("hello.txt")
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()

	content, err := io.ReadAll(file)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello world"), content)
}

func TestWriteEmptyFile(t *testing.T) {
	mfs := memfs.NewMemFS()

	mfs.WriteFile("empty", []byte{})

	file, err := mfs.Open("empty")
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()

	content, err := io.ReadAll(file)
	require.NoError(t, err)
	assert.Empty(t, content)

	stat, err := file.Stat()
	require.NoError(t, err)
	assert.Equal(t, int64(0), stat.Size())
}

func TestWriteOverwritesExistingFile(t *testing.T) {
	mfs := memfs.NewMemFS()

	mfs.WriteFile("file", []byte("original"))
	mfs.WriteFile("file", []byte("updated"))

	file, err := mfs.Open("file")
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()

	content, err := io.ReadAll(file)
	require.NoError(t, err)
	assert.Equal(t, []byte("updated"), content)
}

func TestWriteDefensiveCopy(t *testing.T) {
	mfs := memfs.NewMemFS()

	data := []byte("original")
	mfs.WriteFile("file", data)

	// Mutating the original slice should not affect the stored file
	data[0] = 'X'

	file, err := mfs.Open("file")
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()

	content, err := io.ReadAll(file)
	require.NoError(t, err)
	assert.Equal(t, []byte("original"), content)
}

func TestMultipleFiles(t *testing.T) {
	mfs := memfs.NewMemFS()

	files := map[string][]byte{
		"a.conf": []byte("rule-a"),
		"b.conf": []byte("rule-b"),
		"c.conf": []byte("rule-c"),
	}

	for name, data := range files {
		mfs.WriteFile(name, data)
	}

	for name, expected := range files {
		file, err := mfs.Open(name)
		require.NoError(t, err)
		defer func() { require.NoError(t, file.Close()) }()

		content, err := io.ReadAll(file)
		require.NoError(t, err)
		assert.Equal(t, expected, content, "file %s content mismatch", name)
	}
}

func TestFileStatMetadata(t *testing.T) {
	mfs := memfs.NewMemFS()

	mfs.WriteFile("test.dat", []byte("12345"))

	file, err := mfs.Open("test.dat")
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()

	stat, err := file.Stat()
	require.NoError(t, err)

	assert.Equal(t, "test.dat", stat.Name())
	assert.Equal(t, int64(5), stat.Size())
	assert.Equal(t, fs.FileMode(0644), stat.Mode())
	assert.False(t, stat.IsDir())
	assert.Nil(t, stat.Sys())
	assert.WithinDuration(t, time.Now(), stat.ModTime(), time.Second)
}

func TestWriteFileWithModTime(t *testing.T) {
	mfs := memfs.NewMemFS()

	fixedTime := time.Date(2020, 6, 15, 12, 0, 0, 0, time.UTC)
	mfs.WriteFileWithModTime("stamped.dat", []byte("content"), fixedTime)

	file, err := mfs.Open("stamped.dat")
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()

	stat, err := file.Stat()
	require.NoError(t, err)
	assert.True(t, stat.ModTime().Equal(fixedTime), "expected exact timestamp %v, got %v", fixedTime, stat.ModTime())
}

func TestFileClose(t *testing.T) {
	mfs := memfs.NewMemFS()

	mfs.WriteFile("file", []byte("data"))

	file, err := mfs.Open("file")
	require.NoError(t, err)

	assert.NoError(t, file.Close())
	// Closing multiple times should not error
	assert.NoError(t, file.Close())
}

func TestOpenReturnsSeparateReaders(t *testing.T) {
	mfs := memfs.NewMemFS()

	mfs.WriteFile("file", []byte("shared data"))

	f1, err := mfs.Open("file")
	require.NoError(t, err)
	defer func() { require.NoError(t, f1.Close()) }()

	f2, err := mfs.Open("file")
	require.NoError(t, err)
	defer func() { require.NoError(t, f2.Close()) }()

	// Read from first handle
	c1, err := io.ReadAll(f1)
	require.NoError(t, err)

	// Second handle should still read fully, independent of first
	c2, err := io.ReadAll(f2)
	require.NoError(t, err)

	assert.Equal(t, c1, c2)
	assert.Equal(t, []byte("shared data"), c1)
}

func TestFSInterfaceCompliance(t *testing.T) {
	mfs := memfs.NewMemFS()

	// MemFS should satisfy the fs.FS interface
	var _ fs.FS = mfs
}

func TestConcurrentAccess(t *testing.T) {
	mfs := memfs.NewMemFS()
	const goroutines = 50

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Concurrent writers
	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			mfs.WriteFile("shared", []byte("data"))
			_ = i
		}(i)
	}

	// Concurrent readers
	for range goroutines {
		go func() {
			defer wg.Done()
			file, err := mfs.Open("shared")
			if err != nil {
				// File may not exist yet, that's fine
				return
			}
			defer func() { assert.NoError(t, file.Close()) }()
			_, err = io.ReadAll(file)
			assert.NoError(t, err)
		}()
	}

	wg.Wait()

	// After all goroutines finish, file should be readable
	file, err := mfs.Open("shared")
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()

	content, err := io.ReadAll(file)
	require.NoError(t, err)
	assert.Equal(t, []byte("data"), content)
}

func TestReadAfterWrite(t *testing.T) {
	mfs := memfs.NewMemFS()

	mfs.WriteFile("file", []byte("v1"))

	// Read v1
	f1, err := mfs.Open("file")
	require.NoError(t, err)
	c1, err := io.ReadAll(f1)
	require.NoError(t, err)
	require.NoError(t, f1.Close())
	assert.Equal(t, []byte("v1"), c1)

	// Overwrite to v2
	mfs.WriteFile("file", []byte("v2-longer"))

	// Read v2
	f2, err := mfs.Open("file")
	require.NoError(t, err)
	c2, err := io.ReadAll(f2)
	require.NoError(t, err)
	require.NoError(t, f2.Close())
	assert.Equal(t, []byte("v2-longer"), c2)
}

func TestPartialRead(t *testing.T) {
	mfs := memfs.NewMemFS()

	mfs.WriteFile("file", []byte("abcdefghij"))

	file, err := mfs.Open("file")
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()

	// Read first 4 bytes
	buf := make([]byte, 4)
	n, err := file.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, []byte("abcd"), buf)

	// Read next 4 bytes
	n, err = file.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, []byte("efgh"), buf)

	// Read remaining 2 bytes
	n, err = file.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, []byte("ij"), buf[:n])

	// Next read should return EOF
	n, err = file.Read(buf)
	assert.Equal(t, 0, n)
	assert.ErrorIs(t, err, io.EOF)
}

func TestBinaryData(t *testing.T) {
	mfs := memfs.NewMemFS()

	data := []byte{0x00, 0x01, 0xFF, 0xFE, 0x80, 0x7F}
	mfs.WriteFile("binary.dat", data)

	file, err := mfs.Open("binary.dat")
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()

	content, err := io.ReadAll(file)
	require.NoError(t, err)
	assert.Equal(t, data, content)
}

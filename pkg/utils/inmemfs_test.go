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

package utils_test

import (
	"io"
	"io/fs"
	"testing"
	"time"

	"github.com/networking-incubator/coraza-kubernetes-operator/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInMemFS(t *testing.T) {
	memfs := utils.NewMemFS()

	name := "something"
	data := []byte("somedata")
	memfs.WriteFile(name, data)

	t.Run("should be able to read data", func(t *testing.T) {
		notfound, err := memfs.Open("notfound")
		assert.Nil(t, notfound)
		assert.ErrorIs(t, err, fs.ErrNotExist)
		file, err := memfs.Open(name)
		require.NoError(t, err)
		stat, err := file.Stat()
		require.NoError(t, err)
		now := time.Now()
		assert.Equal(t, name, stat.Name())
		assert.Equal(t, fs.FileMode(0644), stat.Mode())
		assert.WithinDuration(t, now, stat.ModTime(), time.Second)
		assert.False(t, stat.IsDir())
		assert.Nil(t, stat.Sys())
		assert.Equal(t, int64(len(data)), stat.Size())
		content, err := io.ReadAll(file)
		require.NoError(t, err)
		assert.Len(t, content, int(stat.Size()))
		assert.Equal(t, data, content)
		require.NoError(t, file.Close()) // Not deferred because we want to test this
	})

}

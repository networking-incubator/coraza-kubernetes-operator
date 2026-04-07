package controller

import (
	"io/fs"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/memfs"
)

// getDataFilesystem converts data file entries into an in-memory filesystem for Coraza.
// Returns nil if dataFiles is nil.
func getDataFilesystem(dataFiles map[string][]byte) fs.FS {
	if dataFiles == nil {
		return nil
	}

	mfs := memfs.NewMemFS()
	for filename, data := range dataFiles {
		mfs.WriteFile(filename, data)
	}

	return mfs
}

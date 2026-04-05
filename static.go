package mono

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
)

func Folder(path string) Endpoint {
	// TODO: support file list for Folder.
	return &implFolder{dir: path}
}

func Static(filename string) Endpoint {
	return &implStatic{filename: filename}
}

type implStatic struct {
	filename string
}

var _ Endpoint = &implStatic{}

func (static *implStatic) Endpoint(endpoints Endpoints) error {
	return staticFileEndpoint(endpoints, static.filename, static.filename)
}

type implFolder struct {
	err error
	dir string
}

var _ Endpoint = &implFolder{}

func (folder *implFolder) Endpoint(endpoints Endpoints) error {
	stat, err := os.Stat(folder.dir)
	if err != nil {
		return err
	}
	if !stat.IsDir() {
		if err := staticFileEndpoint(endpoints, folder.dir, folder.dir); err != nil {
			return err
		}
	}

	if folder.err != nil {
		return folder.err
	}
	return filepath.WalkDir(folder.dir, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(folder.dir, path)
		if err != nil {
			return err
		}
		return staticFileEndpoint(endpoints, path, relPath)
	})
}

func staticFileEndpoint(endpoints Endpoints, filename string, link string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	endpoints[link] = func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
		if _, err := rw.Write(data); err != nil {
			return err
		}
		return nil
	}
	return nil
}

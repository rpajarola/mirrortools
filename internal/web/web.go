package web

import (
	"embed"
	"io/fs"
)

//go:embed dist/*
var distFS embed.FS

// FS returns an fs.FS serving the embedded Web UI assets.
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}

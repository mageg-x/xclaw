package webui

import (
	"embed"
	"io/fs"
	"path"
	"strings"
)

// Assets stores embedded Web static files.
//
//go:embed all:dist
var Assets embed.FS

func SubFS() (fs.FS, error) {
	return fs.Sub(Assets, "dist")
}

func Exists(relPath string) bool {
	p := strings.TrimPrefix(path.Clean("/"+strings.TrimSpace(relPath)), "/")
	if p == "" || p == "." {
		p = "index.html"
	}
	if !fs.ValidPath(p) {
		return false
	}
	_, err := fs.Stat(Assets, "dist/"+p)
	return err == nil
}

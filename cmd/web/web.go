package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed dist
var embeddedFiles embed.FS

// AssetHandler returns an HTTP handler serving the embedded WebUI.
func AssetHandler() http.Handler {
	distFS, err := fs.Sub(embeddedFiles, "dist")
	if err != nil {
		return NewHandler(nil)
	}
	return NewHandler(distFS)
}

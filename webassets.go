// Package webassets embeds the initial web dashboard (Phase D).
package webassets

import (
	"embed"
	"io/fs"
)

//go:embed all:web
var FS embed.FS

// Sub exposes the web directory as the root so the dashboard is served at "/".
var Sub, _ = fs.Sub(FS, "web")

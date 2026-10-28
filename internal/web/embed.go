// Package web embeds the admin console, browser extension and version data so
// the service can run as a single standalone binary.
package web

import (
	"embed"
	"encoding/json"
	"io/fs"
)

//go:embed version.json
var versionJSON []byte

//go:embed admin.html
var AdminHTML []byte

//go:embed all:extension
var extensionFS embed.FS

// VersionData returns the embedded version.json object.
func VersionData() map[string]any {
	var out map[string]any
	if json.Unmarshal(versionJSON, &out) != nil {
		return map[string]any{"version": "2.0.0", "highlights": []any{}, "highlights_en": []any{}}
	}
	return out
}

// ExtensionFS exposes the embedded extension directory rooted at "extension".
func ExtensionFS() fs.FS {
	sub, err := fs.Sub(extensionFS, "extension")
	if err != nil {
		return extensionFS
	}
	return sub
}

// Package web embeds the dashboard assets into the binary.
package web

import "embed"

// FS holds the copied UI; it is served with no-store from memory.
//
//go:embed index.html favicon.svg fonts.css js styles fonts
var FS embed.FS

// Package docs holds the user documentation. It is embedded in the binary
// and shown by the server under /docs/, so the pages the interface links to
// always match the version that is running.
package docs

import "embed"

//go:embed *.md
var Files embed.FS

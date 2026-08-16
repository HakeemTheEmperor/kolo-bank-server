// Package docs embeds the developer-facing API reference (api-reference.html)
// so internal/httpserver can serve it directly at GET /docs — the same file
// that lives in this directory for anyone reading it straight from the repo.
package docs

import _ "embed"

//go:embed api-reference.html
var APIReferenceHTML []byte

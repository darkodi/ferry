// Package migrations embeds the goose SQL migrations so binaries and tests
// apply exactly the files in this directory.
package migrations

import "embed"

// FS holds every *.sql migration in this directory.
//
//go:embed *.sql
var FS embed.FS

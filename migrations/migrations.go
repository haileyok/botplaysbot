// Package migrations embeds the goose SQL migration files so binaries can run
// schema upgrades without shipping a migrations directory. The returned
// value is goose.SetBaseFS-compatible.
package migrations

import "embed"

//go:embed *.sql
var Files embed.FS

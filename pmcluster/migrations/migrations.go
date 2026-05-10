// Package migrations exposes the embedded SQL migration files used by the
// store package. Migrations live at the top level (rather than inside store/)
// so they're discoverable from a quick repo scan.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

// Package schema holds pgmandate's migrations. From 0.1.0 on, a migration
// that has reached main is never changed; a change is a new file with the
// next number. Before 0.1.0 nothing is kept compatible.
package schema

import (
	"embed"

	"github.com/andreas-lindfalk/pgmandate/pkg/db"
)

//go:embed *.sql
var files embed.FS

// Migrations is pgmandate's own migration set. A host applies it before its own.
var Migrations = db.Migrations{Name: "mandate", Fs: files, Dir: "."}

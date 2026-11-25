// Package schema holds mandate's migrations. A migration that has reached
// main is never changed; a change is a new file with the next number.
package schema

import (
	"embed"

	"github.com/andreas-lindfalk/pgmandate/pkg/db"
)

//go:embed *.sql
var files embed.FS

// Migrations is mandate's own migration set. A host applies it before its own.
var Migrations = db.Migrations{Name: "mandate", Fs: files, Dir: "."}

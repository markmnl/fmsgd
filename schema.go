// Package fmsgd exposes the bootstrap schema for offline maintenance tools.
package fmsgd

import _ "embed"

// Schema is the schema for a new message database. The daemon does not apply it.
//
//go:embed dd.sql
var Schema string

// Package db provides a SQLite database layer with pluggable drivers.
//
// Two SQLite drivers ship with libfossil — modernc (pure Go, default)
// and ncruces (WASM-capable). Select one at build time by importing
// its driver package:
//
//	import _ "github.com/danmestas/go-libfossil/db/driver/modernc"
//
// [Open] and [OpenWith] handle DSN construction, WAL/pragma setup, and
// WASM-specific workarounds. Use [DB.WithTx] for transaction scoping
// with automatic rollback on error.
//
// The [Querier] interface (Exec, QueryRow, Query) is satisfied by both
// [DB] and [Tx], allowing callers to write transaction-agnostic code.
package db

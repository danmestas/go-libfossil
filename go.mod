module github.com/danmestas/go-libfossil

go 1.26.0

require (
	github.com/alecthomas/kong v1.15.0
	github.com/danmestas/go-libfossil/db/driver/modernc v0.1.0
	github.com/danmestas/go-libfossil/db/driver/ncruces v0.1.0
	github.com/hexops/gotextdiff v1.0.3
	golang.org/x/crypto v0.52.0
)

require (
	github.com/danmestas/go-sqlite3-opfs v0.2.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-sqlite3 v0.33.0 // indirect
	github.com/ncruces/go-sqlite3-wasm v1.0.1-0.20260321101821-261d0f98d39c // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/ncruces/julianday v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/mod v0.33.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	modernc.org/libc v1.67.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.46.1 // indirect
)

// The db/driver submodules are versioned in-repo and are not yet published under
// the go-libfossil path, so resolve them from their local directories for
// non-workspace (GOWORK=off) builds. Replace directives are ignored by
// downstream consumers, which resolve the published modules.
//
// Temporary. The Release workflow refuses to cut a tag while any replace
// directive is present, because a tagged module carrying them breaks
// `go install ...@<tag>`. When these are dropped, also clear the stale
// old-path (libfossil) lines still carried in the go.sum files.
replace (
	github.com/danmestas/go-libfossil/db/driver/modernc => ./db/driver/modernc
	github.com/danmestas/go-libfossil/db/driver/ncruces => ./db/driver/ncruces
)

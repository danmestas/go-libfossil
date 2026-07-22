module github.com/danmestas/go-libfossil/db/driver/ncruces

go 1.26.0

require (
	github.com/danmestas/go-libfossil v0.1.0
	github.com/danmestas/go-sqlite3-opfs v0.2.0
	github.com/ncruces/go-sqlite3 v0.33.0
)

require (
	github.com/ncruces/go-sqlite3-wasm v1.0.1-0.20260321101821-261d0f98d39c // indirect
	github.com/ncruces/julianday v1.0.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)

// Resolve in-repo modules from local directories; they are not yet published
// under the go-libfossil path. Ignored by downstream consumers.
//
// Temporary; the Release workflow refuses to cut a tag while any replace
// directive is present. When these are dropped, also clear the stale
// old-path (libfossil) lines still carried in go.sum.
replace (
	github.com/danmestas/go-libfossil => ../../..
	github.com/danmestas/go-libfossil/db/driver/modernc => ../modernc
)

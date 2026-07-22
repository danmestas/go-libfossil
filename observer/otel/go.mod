module github.com/danmestas/go-libfossil/observer/otel

go 1.26.0

require (
	github.com/danmestas/go-libfossil v0.1.0
	go.opentelemetry.io/otel v1.35.0
	go.opentelemetry.io/otel/metric v1.35.0
	go.opentelemetry.io/otel/trace v1.35.0
)

require (
	github.com/go-logr/logr v1.4.2 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
)

// Resolve in-repo modules from local directories; they are not yet published
// under the go-libfossil path. Ignored by downstream consumers.
//
// Temporary; the Release workflow refuses to cut a tag while any replace
// directive is present. When these are dropped, also clear the stale
// old-path (libfossil) lines still carried in go.sum.
replace (
	github.com/danmestas/go-libfossil => ../..
	github.com/danmestas/go-libfossil/db/driver/modernc => ../../db/driver/modernc
	github.com/danmestas/go-libfossil/db/driver/ncruces => ../../db/driver/ncruces
)

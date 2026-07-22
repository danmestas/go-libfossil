module github.com/danmestas/go-libfossil/db/driver/modernc

go 1.26.0

require (
	github.com/danmestas/go-libfossil v0.1.0
	modernc.org/sqlite v1.46.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/tools v0.42.0 // indirect
	modernc.org/libc v1.67.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// Resolve in-repo modules from local directories; they are not yet published
// under the go-libfossil path. Ignored by downstream consumers.
replace (
	github.com/danmestas/go-libfossil => ../../..
	github.com/danmestas/go-libfossil/db/driver/ncruces => ../ncruces
)

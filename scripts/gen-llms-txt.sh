#!/usr/bin/env bash
set -euo pipefail

SITE_DIR="docs/site"
STATIC_DIR="$SITE_DIR/static"

mkdir -p "$STATIC_DIR"

# llms.txt: curated summary
cat > "$STATIC_DIR/llms.txt" << 'HEADER'
# libfossil

> Pure-Go library and CLI for Fossil SCM repositories.
> Zero CGo. Static binaries. WASM-ready.

## Key Concepts
- Repo — open .fossil SQLite-backed repository handle
- Transport — pluggable sync interface (HTTP, custom)
- Driver — pluggable SQLite implementation (modernc, ncruces)
- Observer — hooks for sync/checkout/commit (OpenTelemetry-ready)
- DST — deterministic simulation harness for fault injection

## Quick Start
1. go install github.com/danmestas/go-libfossil/cmd/libfossil@latest
2. libfossil clone <url> repo.fossil
3. Open from Go: repo, err := libfossil.Open("repo.fossil")
4. Sync: repo.Sync(ctx, transport)

## Go SDK Packages
- libfossil — Repo handle, Open/Create, public API
- cli/ — Kong command structs (embeddable)
- db/ — SQLite abstraction with pluggable drivers
- db/driver/modernc — pure-Go SQLite (default)
- db/driver/ncruces — Wasm-friendly SQLite
- observer/otel — OpenTelemetry integration
- dst/ — deterministic simulation harness

## Documentation
Full docs: https://github.com/danmestas/go-libfossil/tree/main/docs/site
HEADER

# llms-full.txt: all content concatenated, frontmatter stripped
echo "# libfossil Documentation (Full)" > "$STATIC_DIR/llms-full.txt"
echo "" >> "$STATIC_DIR/llms-full.txt"
echo "Generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$STATIC_DIR/llms-full.txt"
echo "" >> "$STATIC_DIR/llms-full.txt"

find "$SITE_DIR/content" -name "*.md" -type f | sort | while read -r file; do
    echo "---" >> "$STATIC_DIR/llms-full.txt"
    echo "# Source: $file" >> "$STATIC_DIR/llms-full.txt"
    echo "" >> "$STATIC_DIR/llms-full.txt"
    awk 'NR==1 && /^---$/ { in_fm=1; next }
     in_fm && /^---$/ { in_fm=0; next }
     !in_fm' "$file" >> "$STATIC_DIR/llms-full.txt"
    echo "" >> "$STATIC_DIR/llms-full.txt"
done

echo "Generated llms.txt and llms-full.txt in $STATIC_DIR/"

#!/usr/bin/env bash
# Regenerates the SDK reference: one gomarkdoc page per public package, then
# the Hugo frontmatter that titles each page by its package. Run from the repo
# root. OUT_DIR redirects the output; scripts/check-docs-generated.sh uses it
# to regenerate into a scratch dir and compare.
#
# gomarkdoc runs at the version pinned here through `go run`, so CI needs no
# separate install and every machine renders the same bytes.
set -euo pipefail

OUT_DIR="${OUT_DIR:-docs/site/content/docs/reference/sdk}"
GOMARKDOC="github.com/princjef/gomarkdoc/cmd/gomarkdoc@v1.1.0"

# Package directory, then its page under OUT_DIR.
packages=(
    "./ libfossil/api.md"
    "./cli/ cli/api.md"
    "./db/ db/api.md"
    "./observer/otel/ observer/otel/api.md"
    "./dst/ dst/api.md"
    "./sshverify/ sshverify/api.md"
)
for entry in "${packages[@]}"; do
    read -r pkg page <<<"$entry"
    mkdir -p "$(dirname "$OUT_DIR/$page")"
    GOWORK=off go run "$GOMARKDOC" --output "$OUT_DIR/$page" "$pkg"
done

SDK_ROOT="$OUT_DIR" bash scripts/prepend-sdk-frontmatter.sh

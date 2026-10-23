#!/usr/bin/env bash
# Builds the docs site for deploy: writes the release version and Go version
# into hugo.yaml, regenerates llms*.txt, then runs Hugo in docs/site. Run from
# the repo root. $1 is the hugo binary, resolved from docs/site (default:
# `hugo` on PATH); the Cloudflare build passes ../../hugo.
#
# Needs no Go toolchain. The generated inputs that do (data/*.json, the CLI
# reference) are committed, and CI fails when they are stale.
set -euo pipefail

HUGO="${1:-hugo}"

bash scripts/sync-version.sh
bash scripts/gen-llms-txt.sh
cd docs/site
"$HUGO" --gc --minify

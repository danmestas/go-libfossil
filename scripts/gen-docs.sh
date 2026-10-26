#!/usr/bin/env bash
# Regenerates every generated input of the docs site:
#
#   docs/site/data/engine.json        the landing page's hero figure (tools/docsgen/engine)
#   docs/site/data/conformance.json   per-draft coverage from TestCoverage (tools/docsgen/conformance)
#   docs/site/data/facts.json         the reference fossil version CI pins, and the fuzz target count
#   docs/site/content/docs/reference/cli/   the CLI reference (scripts/gen-cli-docs.sh)
#   docs/site/content/docs/reference/sdk/   the SDK reference (scripts/gen-sdk-docs.sh)
#   docs/site/static/llms*.txt        llms.txt and llms-full.txt (scripts/gen-llms-txt.sh)
#
# Run from the repo root. DATA_DIR, CLI_DIR, SDK_DIR and LLMS_DIR redirect the output;
# scripts/check-docs-generated.sh uses them to regenerate into a scratch dir.
set -euo pipefail

DATA_DIR="${DATA_DIR:-docs/site/data}"
CLI_DIR="${CLI_DIR:-docs/site/content/docs/reference/cli}"
SDK_DIR="${SDK_DIR:-docs/site/content/docs/reference/sdk}"
LLMS_DIR="${LLMS_DIR:-docs/site/static}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT INT TERM
mkdir -p "$DATA_DIR"

GOWORK=off go run ./tools/docsgen/engine -out "$DATA_DIR/engine.json"

CONFORMANCE_REPORT="$work/report.md" GOWORK=off \
    go test ./conformance -run '^TestCoverage$' -count=1 >/dev/null
GOWORK=off go run ./tools/docsgen/conformance \
    -report "$work/report.md" -out "$DATA_DIR/conformance.json"

# The fossil release the interop tests run against is the one CI pins.
fossil_version="$(sed -n 's/^ *FOSSIL_VERSION: "\(.*\)"$/\1/p' .github/workflows/test.yml)"
[ -n "$fossil_version" ] || { echo "gen-docs: no FOSSIL_VERSION in .github/workflows/test.yml" >&2; exit 1; }
fuzz_targets="$(grep -rhE --include='*_test.go' '^func Fuzz[A-Z]' . | wc -l | tr -d ' ')"
printf '{\n  "fossilVersion": "%s",\n  "fuzzTargets": %d\n}\n' "$fossil_version" "$fuzz_targets" \
    > "$DATA_DIR/facts.json"

OUT_DIR="$CLI_DIR" ./scripts/gen-cli-docs.sh >/dev/null
OUT_DIR="$SDK_DIR" ./scripts/gen-sdk-docs.sh >/dev/null
OUT_DIR="$LLMS_DIR" ./scripts/gen-llms-txt.sh >/dev/null

echo "gen-docs: wrote $DATA_DIR/{engine,conformance,facts}.json, $CLI_DIR/, $SDK_DIR/ and $LLMS_DIR/llms*.txt"

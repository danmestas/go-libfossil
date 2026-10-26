#!/usr/bin/env bash
# Fails when a generated docs input differs from what is committed: the hero
# figure and conformance data, the CLI and SDK references and llms*.txt. Regenerates
# everything into a scratch dir with scripts/gen-docs.sh and compares. Run
# from the repo root (CI and `make docs-check` do). Fix a failure with
# `make docs-gen` and commit the result.
set -euo pipefail

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT INT TERM

DATA_DIR="$work/data" CLI_DIR="$work/cli" SDK_DIR="$work/sdk" LLMS_DIR="$work/static" \
    ./scripts/gen-docs.sh >/dev/null

fail=0
for f in engine.json conformance.json facts.json; do
    if ! diff -u "docs/site/data/$f" "$work/data/$f" >&2; then
        echo "check-docs-generated: docs/site/data/$f is stale" >&2
        fail=1
    fi
done
if ! diff -ru docs/site/content/docs/reference/cli "$work/cli" >&2; then
    echo "check-docs-generated: docs/site/content/docs/reference/cli/ is stale" >&2
    fail=1
fi
if ! diff -ru docs/site/content/docs/reference/sdk "$work/sdk" -x _index.md -x driver >&2; then
    echo "check-docs-generated: docs/site/content/docs/reference/sdk/ is stale" >&2
    fail=1
fi
for f in llms.txt llms-full.txt; do
    if ! diff -u "docs/site/static/$f" "$work/static/$f" >&2; then
        echo "check-docs-generated: docs/site/static/$f is stale" >&2
        fail=1
    fi
done

if [ "$fail" -ne 0 ]; then
    echo "check-docs-generated: run 'make docs-gen' and commit the result" >&2
    exit 1
fi
echo "check-docs-generated: ok"

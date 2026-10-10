#!/bin/sh
# Copy the Fossil drafts and errata the conformance suite is written against
# from a zit checkout into conformance/rfcs, unmodified.
#
# A refreshed draft changes its SHA-256, so the coverage test fails until each
# requirements table that pins it has been checked against the new text and
# its pin moved. That is the point: a draft change must be read, not absorbed.
#
# Usage: scripts/refresh-conformance-rfcs.sh [path-to-zit-rfcs]
set -eu
src=${1:-"$HOME/zit/docs/rfcs"}
dst=$(dirname "$0")/../conformance/rfcs
for draft in artifact-format delta-format sync-protocol repo-state; do
	cp "$src/draft-fossil-$draft-00.txt" "$dst/"
done
mkdir -p "$dst/erratum"
cp "$src"/erratum/*.md "$dst/erratum/"
shasum -a 256 "$dst"/*.txt "$dst"/erratum/*.md

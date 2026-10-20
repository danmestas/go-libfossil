#!/bin/sh
# Copy the Fossil drafts the conformance suite is written against from a zit
# checkout into conformance/rfcs, unmodified. The draft text is the authority:
# each draft records its corrections in its own Changes appendix.
#
# A refreshed draft changes its SHA-256, so the coverage test fails until each
# requirements table that pins it has been checked against the new text and
# its pin moved. That is the point: a draft change must be read, not absorbed.
#
# The sparse-cone and horizon drafts follow zit's horizon-rfc branch; pass
# that checkout's docs/rfcs as the second argument to refresh them too (see
# conformance/rfcs/README.md).
#
# Usage: scripts/refresh-conformance-rfcs.sh [path-to-zit-rfcs [path-to-horizon-rfc-rfcs]]
set -eu
src=${1:-"$HOME/zit/docs/rfcs"}
branch=${2:-}
dst=$(dirname "$0")/../conformance/rfcs
for draft in artifact-format sync-protocol repo-state delta-format submanifests; do
	cp "$src/draft-fossil-$draft-00.txt" "$dst/"
done
if [ -n "$branch" ]; then
	for draft in sparse-cone horizon; do
		cp "$branch/draft-fossil-$draft-00.txt" "$dst/"
	done
fi
shasum -a 256 "$dst"/*.txt

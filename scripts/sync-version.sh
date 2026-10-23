#!/usr/bin/env bash
# Writes the latest release tag into docs/site/hugo.yaml's params.version and
# the go directive of go.mod into params.goVersion, so the site shows both
# without a hand edit. Run by the Cloudflare build before Hugo invokes; safe
# to run locally too — it only edits the working tree, never commits.
#
# Leaves params.version untouched if no release tag can be found.
set -euo pipefail

HUGO_YAML="${HUGO_YAML:-docs/site/hugo.yaml}"

# Cloudflare Workers Builds does a shallow clone with no tags, so
# 'git describe' finds nothing: no tag is reachable from a single commit.
# Take the highest release tag by version instead, from local tags if any,
# else from the remote's tag list (the repo is public).
latest_tag() {
    git tag -l 'v*' | sort -V | tail -n 1
}
remote_latest_tag() {
    git ls-remote --tags --refs "$1" 'v*' 2>/dev/null \
        | sed 's#.*refs/tags/##' | sort -V | tail -n 1
}

TAG=$(latest_tag)
if [ -z "${TAG}" ]; then
    TAG=$(remote_latest_tag origin)
fi
if [ -z "${TAG}" ]; then
    TAG=$(remote_latest_tag https://github.com/danmestas/go-libfossil.git)
fi
VERSION="${TAG#v}"

GO_VERSION=$(awk '$1 == "go" { print $2; exit }' go.mod)
[ -n "${GO_VERSION}" ] || { echo "sync-version: no go directive in go.mod" >&2; exit 1; }

# Replace the two lines. awk + temp file stays portable across GNU sed
# (Linux/CF) and BSD sed (macOS).
awk -v v="${VERSION}" -v g="${GO_VERSION}" '
    /^  version: "/ && v != "" { print "  version: \"" v "\""; next }
    /^  goVersion: "/ { print "  goVersion: \"" g "\""; next }
    { print }
' "${HUGO_YAML}" > "${HUGO_YAML}.tmp"
mv "${HUGO_YAML}.tmp" "${HUGO_YAML}"

if [ -z "${VERSION}" ]; then
    echo "sync-version: no release tags found, params.version left as is" >&2
else
    echo "sync-version: params.version set to ${VERSION}"
fi
echo "sync-version: params.goVersion set to ${GO_VERSION}"

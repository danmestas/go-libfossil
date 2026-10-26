#!/usr/bin/env bash
# Prepend Hugo frontmatter to gomarkdoc-generated SDK pages so each page's
# nav label is the package name instead of the filename ("api"). Idempotent:
# strips any existing frontmatter and re-injects, so re-running after
# `make docs-gen-sdk` produces stable bytes.
set -euo pipefail

SDK_ROOT="${SDK_ROOT:-docs/site/content/docs/reference/sdk}"

while IFS= read -r f; do
    rel="${f#${SDK_ROOT}/}"
    pkg="${rel%/api.md}"

    if head -1 "$f" | grep -qE '^---$'; then
        awk 'NR==1 && /^---$/ { in_fm=1; next }
             in_fm && /^---$/ { in_fm=0; next }
             !in_fm' "$f" > "$f.body"
    else
        cp "$f" "$f.body"
    fi

    {
        printf -- '---\n'
        printf 'title: %s\n' "$pkg"
        printf -- '---\n\n'
        cat "$f.body"
    } > "$f"
    rm "$f.body"
done < <(find "$SDK_ROOT" -name 'api.md' -type f | sort)

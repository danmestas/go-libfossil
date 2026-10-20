package cli

import (
	"encoding/json"
	"os"
)

// writeJSON prints v to stdout as one line of JSON. The shapes it prints are
// the machine interface of snapshot, log and diff, documented in
// docs/site/content/docs/cli-embedding.md; a field is never renamed or
// removed, only added.
func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

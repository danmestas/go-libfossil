//go:build !js

package checkout

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
)

func TestSparseMarkerNativeRefusal(t *testing.T) {
	binary, err := exec.LookPath("fossil")
	if err != nil {
		t.Skip("native fossil unavailable")
	}
	f := newSparseRepo(t, false)
	c := f.open(t, nil, f.c1)
	sparse, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetCone(sparse); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"status", "changes", "commit", "update", "revert", "stash", "clean"} {
		t.Run(command, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, command)
			cmd.Dir = c.Dir()
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "within an open check-out") {
				t.Fatalf("native %s: %v: %s", command, err, output)
			}
		})
	}
	if _, err := c.SetCone(nil); err != nil {
		t.Fatal(err)
	}
	assertCheckoutMarker(t, c.Dir(), PreferredDBName())
}

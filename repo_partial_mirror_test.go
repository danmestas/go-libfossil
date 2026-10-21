package libfossil

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The partial-mirror setting is the public switch for §5.3's clone-origin
// refusal: unset, a sparse store refuses a dense clone; set, it serves one.
func TestPartialMirrorSettingDrivesCloneRefusal(t *testing.T) {
	source, err := Create(filepath.Join(t.TempDir(), "source.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, _, err := source.Commit(CommitOpts{User: "test", Files: []FileToCommit{
		{Name: "src/a", Content: []byte("inside\n")}, {Name: "zzz/b", Content: []byte("outside\n")},
	}}); err != nil {
		t.Fatal(err)
	}
	c, err := NewCone([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SetStoreCone(c); err != nil {
		t.Fatal(err)
	}
	peer := TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		return source.HandleSync(ctx, payload)
	})
	for _, setting := range []string{"", "off", "on"} {
		if setting != "" {
			if err := source.SetConfig(SettingPartialMirror, setting); err != nil {
				t.Fatal(err)
			}
		}
		clone, _, err := Clone(context.Background(),
			filepath.Join(t.TempDir(), "clone-"+setting+".fossil"), peer, CloneOpts{})
		if setting != "on" {
			if err == nil || !strings.Contains(err.Error(), "configure a partial mirror") {
				t.Fatalf("partial-mirror=%q: clone error = %v, want the origin refusal", setting, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("partial-mirror=on: clone refused: %v", err)
		}
		clone.Close()
	}
}

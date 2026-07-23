package content

import (
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func TestIsPrivate_NotPrivate(t *testing.T) {
	d := setupTestDB(t)
	rid, _, _ := blob.Store(d, []byte("public blob"))
	if IsPrivate(d, int64(rid)) {
		t.Error("blob should not be private by default")
	}
}

func TestMakePrivate_ThenIsPrivate(t *testing.T) {
	d := setupTestDB(t)
	rid, _, _ := blob.Store(d, []byte("will be private"))
	if err := MakePrivate(d, int64(rid)); err != nil {
		t.Fatalf("MakePrivate: %v", err)
	}
	if !IsPrivate(d, int64(rid)) {
		t.Error("blob should be private after MakePrivate")
	}
}

func TestMakePrivate_Idempotent(t *testing.T) {
	d := setupTestDB(t)
	rid, _, _ := blob.Store(d, []byte("double private"))
	MakePrivate(d, int64(rid))
	if err := MakePrivate(d, int64(rid)); err != nil {
		t.Fatalf("second MakePrivate should not error: %v", err)
	}
}

func TestMakePublic_ClearsPrivate(t *testing.T) {
	d := setupTestDB(t)
	rid, _, _ := blob.Store(d, []byte("private then public"))
	MakePrivate(d, int64(rid))
	if err := MakePublic(d, int64(rid)); err != nil {
		t.Fatalf("MakePublic: %v", err)
	}
	if IsPrivate(d, int64(rid)) {
		t.Error("blob should not be private after MakePublic")
	}
}

func TestMakePublic_NoopIfNotPrivate(t *testing.T) {
	d := setupTestDB(t)
	rid, _, _ := blob.Store(d, []byte("never private"))
	if err := MakePublic(d, int64(rid)); err != nil {
		t.Fatalf("MakePublic on non-private should not error: %v", err)
	}
}

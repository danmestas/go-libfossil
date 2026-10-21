package vfile

import (
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/simio"
)

// A storage without links (memory, browser) never sees one: a symlink
// entry is written as a plain file holding its target's name, as fossil's
// symlink_create writes it with allow-symlinks off, and permissions are not
// read from it.
func TestDiskWithoutLinks(t *testing.T) {
	env := &simio.Env{
		Clock: simio.NewSimClock(), Rand: simio.NewSeededRand(1), Storage: simio.NewMemStorage(),
	}
	d := NewDisk(env, true)
	if d.ReadsPerms() || d.Symlinks() {
		t.Fatalf("ReadsPerms %v, Symlinks %v on a storage without links", d.ReadsPerms(), d.Symlinks())
	}
	p := filepath.Join("/ck", "sub", "link")
	if err := d.Write(p, []byte("target.txt"), false, true); err != nil {
		t.Fatal(err)
	}
	info, err := d.Stat(p)
	if err != nil || info == nil || IsLink(info) {
		t.Fatalf("Stat = %v, %v; want a plain file", info, err)
	}
	data, err := d.Read(p, info)
	if err != nil || string(data) != "target.txt" {
		t.Fatalf("Read = %q, %v; want the target's name", data, err)
	}
	if err := d.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(p); err != nil {
		t.Fatalf("Remove of nothing = %v, want nil", err)
	}
	if info, err := d.Stat(p); info != nil || err != nil {
		t.Fatalf("Stat after Remove = %v, %v; want nothing", info, err)
	}
}

func TestZeroDiskPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a zero Disk did not panic")
		}
	}()
	var d Disk
	_, _ = d.Stat("/x")
}

package libfossil

import (
	"path/filepath"
	"testing"
)

func adminLogin(t *testing.T, r *Repo) string {
	t.Helper()
	var login string
	if err := r.DB().QueryRow("SELECT login FROM user WHERE uid=1").Scan(&login); err != nil {
		t.Fatalf("read seeded user: %v", err)
	}
	return login
}

// Create with no CreateOpts.User and no user in the environment must still
// create a repository, seeding fossil's fallback login "root", not panic on
// an empty login.
func TestCreate_NoUserAnywhereSeedsRoot(t *testing.T) {
	for _, name := range []string{"USER", "LOGNAME", "USERNAME"} {
		t.Setenv(name, "")
	}
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()
	if got := adminLogin(t, r); got != "root" {
		t.Errorf("seeded login = %q, want root", got)
	}
}

// The environment is consulted in fossil's order: USER, then LOGNAME, then
// USERNAME.
func TestCreate_DefaultLoginEnvOrder(t *testing.T) {
	t.Setenv("USER", "")
	t.Setenv("LOGNAME", "from-logname")
	t.Setenv("USERNAME", "from-username")
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()
	if got := adminLogin(t, r); got != "from-logname" {
		t.Errorf("seeded login = %q, want LOGNAME before USERNAME", got)
	}

	t.Setenv("USER", "from-user")
	r2, err := Create(filepath.Join(t.TempDir(), "r2.fossil"), CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r2.Close()
	if got := adminLogin(t, r2); got != "from-user" {
		t.Errorf("seeded login = %q, want USER first", got)
	}

	// An explicit user always wins.
	r3, err := Create(filepath.Join(t.TempDir(), "r3.fossil"), CreateOpts{User: "explicit"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r3.Close()
	if got := adminLogin(t, r3); got != "explicit" {
		t.Errorf("seeded login = %q, want the explicit user", got)
	}
}

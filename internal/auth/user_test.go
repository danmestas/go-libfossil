package auth

import (
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil/db"
	_ "github.com/danmestas/libfossil/internal/testdriver"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
)

func setupTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.fossil")
	r, err := repo.Create(path, "admin", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r.DB()
}

func projectCode(t *testing.T, d *db.DB) string {
	t.Helper()
	var code string
	if err := d.QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&code); err != nil {
		t.Fatalf("project-code: %v", err)
	}
	return code
}

func TestCreateAndGetUser(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)

	err := CreateUser(d, code, "alice", "secret123", "ios")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	user, err := GetUser(d, "alice")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}

	if user.Login != "alice" {
		t.Errorf("Login = %q, want alice", user.Login)
	}
	if user.Cap != "ios" {
		t.Errorf("Cap = %q, want ios", user.Cap)
	}

	// Verify password hash was stored correctly
	var storedPW string
	err = d.QueryRow("SELECT pw FROM user WHERE login='alice'").Scan(&storedPW)
	if err != nil {
		t.Fatalf("query stored pw: %v", err)
	}
	expected := hashPassword(code, "alice", "secret123")
	if storedPW != expected {
		t.Errorf("stored pw = %q, want %q", storedPW, expected)
	}
}

func TestCreateUserDuplicate(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)

	err := CreateUser(d, code, "alice", "secret123", "o")
	if err != nil {
		t.Fatalf("CreateUser first: %v", err)
	}

	err = CreateUser(d, code, "alice", "different", "i")
	if err == nil {
		t.Fatal("CreateUser duplicate: expected error, got nil")
	}
}

func TestGetUserNotFound(t *testing.T) {
	d := setupTestDB(t)

	_, err := GetUser(d, "doesnotexist")
	if err == nil {
		t.Fatal("GetUser nonexistent: expected error, got nil")
	}
}

func TestListUsers(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)

	err := CreateUser(d, code, "alice", "pw1", "o")
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}

	err = CreateUser(d, code, "bob", "pw2", "i")
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	users, err := ListUsers(d)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}

	// Should have at least 3: admin + alice + bob
	if len(users) < 3 {
		t.Errorf("ListUsers returned %d users, want >= 3", len(users))
	}

	// Verify alice and bob are in the list
	found := make(map[string]bool)
	for _, u := range users {
		found[u.Login] = true
	}
	if !found["alice"] {
		t.Error("alice not in ListUsers result")
	}
	if !found["bob"] {
		t.Error("bob not in ListUsers result")
	}
}

func TestUpdateCaps(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)

	err := CreateUser(d, code, "alice", "pw", "o")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = UpdateCaps(d, "alice", "ois")
	if err != nil {
		t.Fatalf("UpdateCaps: %v", err)
	}

	user, err := GetUser(d, "alice")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}

	if user.Cap != "ois" {
		t.Errorf("Cap after update = %q, want ois", user.Cap)
	}
}

func TestUpdateCapsNotFound(t *testing.T) {
	d := setupTestDB(t)

	err := UpdateCaps(d, "doesnotexist", "o")
	if err == nil {
		t.Fatal("UpdateCaps nonexistent: expected error, got nil")
	}
}

func TestSetPassword(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)

	err := CreateUser(d, code, "alice", "oldpw", "o")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Get original password hash
	var oldPW string
	err = d.QueryRow("SELECT pw FROM user WHERE login='alice'").Scan(&oldPW)
	if err != nil {
		t.Fatalf("query old pw: %v", err)
	}

	err = SetPassword(d, code, "alice", "newpw")
	if err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// Verify password hash changed
	var newPW string
	err = d.QueryRow("SELECT pw FROM user WHERE login='alice'").Scan(&newPW)
	if err != nil {
		t.Fatalf("query new pw: %v", err)
	}

	if newPW == oldPW {
		t.Error("password hash did not change")
	}

	expected := hashPassword(code, "alice", "newpw")
	if newPW != expected {
		t.Errorf("new pw = %q, want %q", newPW, expected)
	}
}

func TestSetPasswordNotFound(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)

	err := SetPassword(d, code, "doesnotexist", "pw")
	if err == nil {
		t.Fatal("SetPassword nonexistent: expected error, got nil")
	}
}

func TestDeleteUser(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)

	err := CreateUser(d, code, "alice", "pw", "o")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = DeleteUser(d, "alice")
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	// Verify user is gone
	_, err = GetUser(d, "alice")
	if err == nil {
		t.Fatal("GetUser after delete: expected error, got nil")
	}
}

func TestDeleteUserNotFound(t *testing.T) {
	d := setupTestDB(t)

	err := DeleteUser(d, "doesnotexist")
	if err == nil {
		t.Fatal("DeleteUser nonexistent: expected error, got nil")
	}
}

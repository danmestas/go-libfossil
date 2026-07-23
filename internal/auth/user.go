package auth

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
)

// hashPassword computes SHA1(projectCode/login/password) matching Fossil's convention.
func hashPassword(projectCode, login, password string) string {
	h := sha1.Sum([]byte(projectCode + "/" + login + "/" + password))
	return hex.EncodeToString(h[:])
}

// CreateUser inserts a new user into the user table.
// Panics if d is nil or login is empty.
func CreateUser(d *db.DB, projectCode, login, password, caps string) error {
	if d == nil {
		panic("auth.CreateUser: d must not be nil")
	}
	if projectCode == "" {
		panic("auth.CreateUser: projectCode must not be empty")
	}
	if login == "" {
		panic("auth.CreateUser: login must not be empty")
	}

	pw := hashPassword(projectCode, login, password)
	_, err := d.Exec(
		"INSERT INTO user(login, pw, cap, mtime) VALUES(?, ?, ?, julianday('now'))",
		login, pw, caps,
	)
	if err != nil {
		return fmt.Errorf("insert user %q: %w", login, err)
	}
	return nil
}

// GetUser retrieves a user by login.
// Returns an error if the user is not found.
// Panics if d is nil.
func GetUser(d *db.DB, login string) (User, error) {
	if d == nil {
		panic("auth.GetUser: d must not be nil")
	}

	var u User
	var cexpire sql.NullString
	var info sql.NullString
	var mtimeRaw any

	err := d.QueryRow(
		"SELECT uid, login, cap, cexpire, info, mtime FROM user WHERE login=?",
		login,
	).Scan(&u.UID, &u.Login, &u.Cap, &cexpire, &info, &mtimeRaw)

	if err != nil {
		if err == sql.ErrNoRows {
			return User{}, fmt.Errorf("user %q not found", login)
		}
		return User{}, fmt.Errorf("query user %q: %w", login, err)
	}

	if info.Valid {
		u.Info = info.String
	}

	return u, nil
}

// ListUsers returns all users ordered by login.
// Panics if d is nil.
func ListUsers(d *db.DB) ([]User, error) {
	if d == nil {
		panic("auth.ListUsers: d must not be nil")
	}

	rows, err := d.Query("SELECT uid, login, cap, info FROM user ORDER BY login")
	if err != nil {
		return nil, fmt.Errorf("query users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		var info sql.NullString
		if err := rows.Scan(&u.UID, &u.Login, &u.Cap, &info); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		if info.Valid {
			u.Info = info.String
		}
		users = append(users, u)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}

	return users, nil
}

// UpdateCaps updates the capabilities for a user.
// Returns an error if the user is not found (0 rows affected).
// Panics if d is nil.
func UpdateCaps(d *db.DB, login, caps string) error {
	if d == nil {
		panic("auth.UpdateCaps: d must not be nil")
	}
	if login == "" {
		panic("auth.UpdateCaps: login must not be empty")
	}

	result, err := d.Exec("UPDATE user SET cap=? WHERE login=?", caps, login)
	if err != nil {
		return fmt.Errorf("update caps for %q: %w", login, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("user %q not found", login)
	}

	return nil
}

// SetPassword updates the password for a user.
// Returns an error if the user is not found (0 rows affected).
// Panics if d is nil.
func SetPassword(d *db.DB, projectCode, login, password string) error {
	if d == nil {
		panic("auth.SetPassword: d must not be nil")
	}
	if projectCode == "" {
		panic("auth.SetPassword: projectCode must not be empty")
	}

	pw := hashPassword(projectCode, login, password)
	result, err := d.Exec("UPDATE user SET pw=? WHERE login=?", pw, login)
	if err != nil {
		return fmt.Errorf("update password for %q: %w", login, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("user %q not found", login)
	}

	return nil
}

// DeleteUser deletes a user by login.
// Returns an error if the user is not found (0 rows affected).
// Panics if d is nil.
func DeleteUser(d *db.DB, login string) error {
	if d == nil {
		panic("auth.DeleteUser: d must not be nil")
	}
	if login == "" {
		panic("auth.DeleteUser: login must not be empty")
	}

	result, err := d.Exec("DELETE FROM user WHERE login=?", login)
	if err != nil {
		return fmt.Errorf("delete user %q: %w", login, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("user %q not found", login)
	}

	return nil
}

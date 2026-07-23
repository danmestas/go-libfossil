package libfossil

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/auth"
)

// UserOpts configures a user creation or update.
type UserOpts struct {
	Login    string
	Password string
	Caps     string
}

// User describes a Fossil user.
type User struct {
	Login string
	Caps  string
}

// CreateUser creates a new user in the repository.
func (r *Repo) CreateUser(opts UserOpts) error {
	projectCode, err := r.Config("project-code")
	if err != nil {
		return fmt.Errorf("libfossil: create user: %w", err)
	}
	return auth.CreateUser(r.inner.DB(), projectCode, opts.Login, opts.Password, opts.Caps)
}

// GetUser returns information about a user.
func (r *Repo) GetUser(login string) (User, error) {
	u, err := auth.GetUser(r.inner.DB(), login)
	if err != nil {
		return User{}, fmt.Errorf("libfossil: get user: %w", err)
	}
	return User{Login: u.Login, Caps: u.Cap}, nil
}

// ListUsers returns all users in the repository.
func (r *Repo) ListUsers() ([]User, error) {
	users, err := auth.ListUsers(r.inner.DB())
	if err != nil {
		return nil, fmt.Errorf("libfossil: list users: %w", err)
	}
	result := make([]User, len(users))
	for i, u := range users {
		result[i] = User{Login: u.Login, Caps: u.Cap}
	}
	return result, nil
}

// DeleteUser removes a user from the repository.
func (r *Repo) DeleteUser(login string) error {
	return auth.DeleteUser(r.inner.DB(), login)
}

// SetPassword updates a user's password.
func (r *Repo) SetPassword(login, password string) error {
	projectCode, err := r.Config("project-code")
	if err != nil {
		return fmt.Errorf("libfossil: set password: %w", err)
	}
	return auth.SetPassword(r.inner.DB(), projectCode, login, password)
}

// SetCaps updates a user's capability string.
func (r *Repo) SetCaps(login, caps string) error {
	return auth.UpdateCaps(r.inner.DB(), login, caps)
}

// Config reads a configuration value from the repo's config table.
func (r *Repo) Config(key string) (string, error) {
	val, err := r.inner.Config(key)
	if err != nil {
		return "", fmt.Errorf("libfossil: config %q: %w", key, err)
	}
	return val, nil
}

// SetConfig writes a configuration value to the repo's config table.
func (r *Repo) SetConfig(key, value string) error {
	if err := r.inner.SetConfig(key, value); err != nil {
		return fmt.Errorf("libfossil: set config %q: %w", key, err)
	}
	return nil
}

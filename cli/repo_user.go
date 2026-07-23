package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	libfossil "github.com/danmestas/go-libfossil"
)

// RepoUserCmd groups user management operations.
type RepoUserCmd struct {
	Add    UserAddCmd    `cmd:"" help:"Create a new user"`
	List   UserListCmd   `cmd:"" help:"List all users"`
	Update UserUpdateCmd `cmd:"" help:"Update user capabilities"`
	Rm     UserRmCmd     `cmd:"" help:"Delete a user"`
	Passwd UserPasswdCmd `cmd:"" help:"Reset user password"`
}

// UserAddCmd creates a new user.
type UserAddCmd struct {
	Login string `arg:"" help:"Username"`
	Cap   string `help:"Capability string (e.g. oi)" required:""`
}

func (c *UserAddCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	password, err := generatePassword()
	if err != nil {
		return err
	}

	if err := r.CreateUser(libfossil.UserOpts{
		Login:    c.Login,
		Password: password,
		Caps:     c.Cap,
	}); err != nil {
		return err
	}
	fmt.Printf("Created user %q (caps: %s)\n", c.Login, c.Cap)
	fmt.Printf("Password: %s\n", password)
	return nil
}

// UserListCmd lists all users.
type UserListCmd struct{}

func (c *UserListCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	users, err := r.ListUsers()
	if err != nil {
		return err
	}
	fmt.Printf("%-20s %-20s\n", "LOGIN", "CAPABILITIES")
	for _, u := range users {
		fmt.Printf("%-20s %-20s\n", u.Login, u.Caps)
	}
	return nil
}

// UserUpdateCmd updates user capabilities.
type UserUpdateCmd struct {
	Login string `arg:"" help:"Username"`
	Cap   string `help:"New capability string" required:""`
}

func (c *UserUpdateCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	if err := r.SetCaps(c.Login, c.Cap); err != nil {
		return err
	}
	fmt.Printf("Updated %q capabilities: %s\n", c.Login, c.Cap)
	return nil
}

// UserRmCmd deletes a user.
type UserRmCmd struct {
	Login string `arg:"" help:"Username"`
}

func (c *UserRmCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	if err := r.DeleteUser(c.Login); err != nil {
		return err
	}
	fmt.Printf("Deleted user %q\n", c.Login)
	return nil
}

// UserPasswdCmd resets a user's password.
type UserPasswdCmd struct {
	Login string `arg:"" help:"Username"`
}

func (c *UserPasswdCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	password, err := generatePassword()
	if err != nil {
		return err
	}

	if err := r.SetPassword(c.Login, password); err != nil {
		return err
	}
	fmt.Printf("New password for %q: %s\n", c.Login, password)
	return nil
}

func generatePassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating password: %w", err)
	}
	return hex.EncodeToString(b), nil
}

package checkout

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danmestas/go-libfossil/internal/content"
)

// FileContent reads a file from the checkout working directory via Storage.
// Panics if c is nil or name is empty (TigerStyle preconditions).
func (c *Checkout) FileContent(name string) ([]byte, error) {
	if c == nil {
		panic("checkout.FileContent: nil *Checkout")
	}
	if name == "" {
		panic("checkout.FileContent: empty name")
	}

	fullPath, err := c.safePath(name)
	if err != nil {
		return nil, fmt.Errorf("checkout.FileContent: %w", err)
	}
	data, err := c.env.Storage.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("checkout.FileContent: %w", err)
	}
	return data, nil
}

// WriteManifest writes manifest and/or manifest.uuid files to the checkout directory.
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) WriteManifest(flags ManifestFlags) error {
	if c == nil {
		panic("checkout.WriteManifest: nil *Checkout")
	}

	// Get current version
	rid, uuid, err := c.Version()
	if err != nil {
		return fmt.Errorf("checkout.WriteManifest: %w", err)
	}

	// Write full manifest file
	if flags&ManifestMain != 0 {
		manifestContent, err := content.Expand(c.repo.DB(), rid)
		if err != nil {
			return fmt.Errorf("checkout.WriteManifest: expand manifest: %w", err)
		}

		manifestPath, err := c.safePath("manifest")
		if err != nil {
			return fmt.Errorf("checkout.WriteManifest: %w", err)
		}
		if err := c.env.Storage.WriteFile(manifestPath, manifestContent, os.FileMode(0o644)); err != nil {
			return fmt.Errorf("checkout.WriteManifest: write manifest: %w", err)
		}
	}

	// Write UUID file
	if flags&ManifestUUID != 0 {
		uuidContent := []byte(uuid + "\n")
		uuidPath, err := c.safePath("manifest.uuid")
		if err != nil {
			return fmt.Errorf("checkout.WriteManifest: %w", err)
		}
		if err := c.env.Storage.WriteFile(uuidPath, uuidContent, os.FileMode(0o644)); err != nil {
			return fmt.Errorf("checkout.WriteManifest: write manifest.uuid: %w", err)
		}
	}

	return nil
}

// safePath validates that name is within the checkout directory and returns
// the full absolute path. Panics if c is nil or name is empty.
func (c *Checkout) safePath(name string) (string, error) {
	clean, err := c.CheckFilename(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(c.dir, clean), nil
}

// CheckFilename canonicalizes and validates that a filename is within the checkout directory.
// Returns the cleaned relative path, or an error if the path escapes the checkout.
// Panics if c is nil or name is empty (TigerStyle preconditions).
func (c *Checkout) CheckFilename(name string) (string, error) {
	if c == nil {
		panic("checkout.CheckFilename: nil *Checkout")
	}
	if name == "" {
		panic("checkout.CheckFilename: empty name")
	}

	// Clean the path
	cleaned := filepath.Clean(name)

	// If absolute, make it relative to checkout dir
	if filepath.IsAbs(cleaned) {
		rel, err := filepath.Rel(c.dir, cleaned)
		if err != nil {
			return "", fmt.Errorf("checkout.CheckFilename: path outside checkout: %s", name)
		}
		cleaned = rel
	}

	// Reject paths that escape the checkout directory
	// filepath.Clean normalizes ".." so we can check for leading ".."
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("checkout.CheckFilename: path escapes checkout: %s", name)
	}

	return cleaned, nil
}

// IsRootedIn checks if an absolute path is within the checkout directory.
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) IsRootedIn(absPath string) bool {
	if c == nil {
		panic("checkout.IsRootedIn: nil *Checkout")
	}

	cleanAbs := filepath.Clean(absPath)
	cleanDir := filepath.Clean(c.dir)

	// Check if absPath is within checkout dir (exact match or separator-bounded prefix)
	return cleanAbs == cleanDir || strings.HasPrefix(cleanAbs, cleanDir+string(filepath.Separator))
}

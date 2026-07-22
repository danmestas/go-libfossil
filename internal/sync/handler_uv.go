package sync

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/uv"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func (h *handler) handlePragmaUVHash(clientHash string) error {
	if h.uvCatalogSent {
		return nil
	}
	if err := uv.EnsureSchema(h.repo.DB()); err != nil {
		return fmt.Errorf("handler.handlePragmaUVHash: ensure schema: %w", err)
	}

	localHash, err := uv.ContentHash(h.repo.DB())
	if err != nil {
		return fmt.Errorf("handler.handlePragmaUVHash: content hash: %w", err)
	}
	if localHash == clientHash {
		return nil // already in sync
	}

	h.uvCatalogSent = true
	h.resp = append(h.resp, &xfer.PragmaCard{Name: "uv-push-ok"})

	entries, err := uv.List(h.repo.DB())
	if err != nil {
		return fmt.Errorf("handler.handlePragmaUVHash: list: %w", err)
	}
	for _, e := range entries {
		hashVal := e.Hash
		if hashVal == "" {
			hashVal = "-"
		}
		h.resp = append(h.resp, &xfer.UVIGotCard{
			Name:  e.Name,
			MTime: e.MTime,
			Hash:  hashVal,
			Size:  e.Size,
		})
	}
	return nil
}

func (h *handler) handleUVIGot(c *xfer.UVIGotCard) error {
	if c == nil {
		panic("handler.handleUVIGot: c must not be nil")
	}
	if err := uv.EnsureSchema(h.repo.DB()); err != nil {
		return fmt.Errorf("handler.handleUVIGot: ensure schema: %w", err)
	}

	_, localMtime, localHash, err := uv.Read(h.repo.DB(), c.Name)
	if err != nil {
		return fmt.Errorf("handler.handleUVIGot: read %q: %w", c.Name, err)
	}

	status := uv.Status(localMtime, localHash, c.MTime, c.Hash)

	switch {
	case status == 0 || status == 1:
		h.resp = append(h.resp, &xfer.UVGimmeCard{Name: c.Name})
	case status == 2:
		if _, err := h.repo.DB().Exec("UPDATE unversioned SET mtime=? WHERE name=?", c.MTime, c.Name); err != nil {
			return fmt.Errorf("handler.handleUVIGot: update mtime %q: %w", c.Name, err)
		}
		if err := uv.InvalidateHash(h.repo.DB()); err != nil {
			return fmt.Errorf("handler.handleUVIGot: invalidate hash: %w", err)
		}
	case status == 4 || status == 5:
		if err := h.sendUVFile(c.Name); err != nil {
			return fmt.Errorf("handler.handleUVIGot: send %q: %w", c.Name, err)
		}
	}
	return nil
}

func (h *handler) handleUVGimme(c *xfer.UVGimmeCard) error {
	if c == nil {
		panic("handler.handleUVGimme: c must not be nil")
	}
	if h.buggify != nil && h.buggify.Check("handler.handleUVGimme.skip", 0.05) {
		return nil
	}
	return h.sendUVFile(c.Name)
}

func (h *handler) sendUVFile(name string) error {
	if name == "" {
		panic("handler.sendUVFile: name must not be empty")
	}
	content, mtime, fileHash, err := uv.Read(h.repo.DB(), name)
	if err != nil {
		return fmt.Errorf("handler.sendUVFile: read %q: %w", name, err)
	}

	if fileHash == "" {
		h.resp = append(h.resp, &xfer.UVFileCard{
			Name:  name,
			MTime: mtime,
			Hash:  "-",
			Size:  0,
			Flags: xfer.UVFlagDeletion,
		})
		return nil
	}

	h.resp = append(h.resp, &xfer.UVFileCard{
		Name:    name,
		MTime:   mtime,
		Hash:    fileHash,
		Size:    len(content),
		Flags:   0,
		Content: content,
	})
	return nil
}

func (h *handler) handleUVFile(c *xfer.UVFileCard) error {
	if c == nil {
		panic("handler.handleUVFile: c must not be nil")
	}
	if !auth.CanPushUV(h.caps) {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("uvfile %s denied: insufficient capabilities", c.Name),
		})
		return nil
	}
	if !h.pushOK {
		h.resp = append(h.resp, &xfer.ErrorCard{
			Message: fmt.Sprintf("uvfile %s rejected: no push card", c.Name),
		})
		return nil
	}

	if h.buggify != nil && h.buggify.Check("handler.handleUVFile.drop", 0.03) {
		return nil
	}

	if err := uv.EnsureSchema(h.repo.DB()); err != nil {
		return fmt.Errorf("handler.handleUVFile: ensure schema: %w", err)
	}

	// Validate hash if content present.
	if c.Flags&xfer.UVFlagNoPayload == 0 {
		if c.Content == nil {
			panic("handler.handleUVFile: flags indicate payload but Content is nil")
		}
		computed := hash.ContentHash(c.Content, c.Hash)
		if computed != c.Hash {
			h.resp = append(h.resp, &xfer.ErrorCard{
				Message: fmt.Sprintf("uvfile %s: hash mismatch", c.Name),
			})
			return nil
		}
	}

	// Double-check status.
	_, localMtime, localHash, err := uv.Read(h.repo.DB(), c.Name)
	if err != nil {
		return fmt.Errorf("handler.handleUVFile: read %q: %w", c.Name, err)
	}

	status := uv.Status(localMtime, localHash, c.MTime, c.Hash)
	if status >= 2 {
		return nil
	}

	// Apply.
	if c.Hash == "-" {
		return uv.Delete(h.repo.DB(), c.Name, c.MTime)
	}
	if c.Content != nil {
		return uv.Write(h.repo.DB(), c.Name, c.Content, c.MTime)
	}
	return nil
}

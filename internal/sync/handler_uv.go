package sync

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/uv"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// handlePragmaUVHash answers pragma uv-hash as fossil's page_xfer does: a
// requester with Read whose hash differs gets the catalog, headed by
// uv-push-ok if it may write unversioned files (y) and uv-pull-only if not.
// Anyone else gets nothing, and the pragma is spent either way.
func (h *handler) handlePragmaUVHash(clientHash string) error {
	if h.uvCatalogSent {
		return nil
	}
	if !auth.CanRead(h.caps) {
		h.uvCatalogSent = true
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
	if auth.CanPushUV(h.caps) {
		h.resp = append(h.resp, &xfer.PragmaCard{Name: "uv-push-ok"})
	} else {
		h.resp = append(h.resp, &xfer.PragmaCard{Name: "uv-pull-only"})
	}

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
	// Fossil's server ignores uvigot; this one answers it, so it holds the
	// answer to the same rule as uvgimme: nothing for a requester without Read.
	if !auth.CanRead(h.caps) {
		return nil
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
		// Adopting the client's mtime is an unversioned write.
		if !auth.CanPushUV(h.caps) {
			return nil
		}
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
	// Fossil 2.28 sends any uvgimme'd file to any requester. Its own client
	// asks only for files named in a uv-hash catalog, which needs Read, so
	// requiring Read here costs no interop and closes the direct read.
	if !auth.CanRead(h.caps) {
		return nil
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

	// §7.6: an unversioned file named by SHA3 is withheld from an older
	// client, as fossil's send_unversioned_file withholds it.
	if sha3Barred(fileHash, h.peerVersion) {
		h.resp = append(h.resp, sha3Refusal())
		return nil
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
	// fossil's xfer_accept_unversioned_file checks the content's HASH, then
	// the write permission; either refusal resets the reply, answers the
	// error and breaks out of the card loop, as page_xfer does (§7.3). Only
	// a card that carries content (sz>0, neither flag 1 nor 4) is checked:
	// a deletion, HASH "-" and size 0, carries none.
	if c.Size > 0 && c.Flags&xfer.UVFlagNoPayload == 0 {
		if c.Content == nil {
			panic("handler.handleUVFile: flags indicate payload but Content is nil")
		}
		if hash.ContentHash(c.Content, c.Hash) != c.Hash {
			h.breakLoop(fmt.Sprintf("uvfile %s: hash mismatch", c.Name))
			return nil
		}
	}
	// The write permission is WrUnver (y) alone, with or without a push card:
	// `fossil uv sync` sends none.
	if !auth.CanPushUV(h.caps) {
		h.breakLoop(fmt.Sprintf("uvfile %s denied: insufficient capabilities", c.Name))
		return nil
	}

	if h.buggify != nil && h.buggify.Check("handler.handleUVFile.drop", 0.03) {
		return nil
	}

	if err := uv.EnsureSchema(h.repo.DB()); err != nil {
		return fmt.Errorf("handler.handleUVFile: ensure schema: %w", err)
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

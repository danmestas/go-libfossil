package sync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"

	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// maxRequestBodyBytes caps the size of incoming xfer request bodies.
// Fossil sync payloads are typically under 1MB; 50MB is generous headroom.
const maxRequestBodyBytes = 50 * 1024 * 1024

// ServeHTTP starts an HTTP server that accepts Fossil xfer requests.
// Blocks until ctx is cancelled. Stock fossil clone/sync can connect.
func ServeHTTP(ctx context.Context, addr string, r *repo.Repo, h HandleFunc) error {
	if addr == "" {
		panic("sync.ServeHTTP: addr must not be empty")
	}
	if r == nil {
		panic("sync.ServeHTTP: r must not be nil")
	}
	if h == nil {
		panic("sync.ServeHTTP: h must not be nil")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", xferHandler(r, h))

	srv := &http.Server{
		Handler: mux,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("sync.ServeHTTP: listen: %w", err)
	}

	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	err = srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// XferHandler returns an http.HandlerFunc that decodes Fossil xfer requests,
// dispatches to the HandleFunc, and encodes the response. Use this to compose
// a custom mux when you need additional routes (e.g. /healthz) alongside xfer.
func XferHandler(r *repo.Repo, h HandleFunc) http.HandlerFunc {
	if r == nil {
		panic("sync.XferHandler: r must not be nil")
	}
	if h == nil {
		panic("sync.XferHandler: h must not be nil")
	}
	return xferHandler(r, h)
}

// xferHandler is the unexported implementation.
func xferHandler(r *repo.Repo, h HandleFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		// Fossil sends GET as a server probe — respond with a basic page.
		if req.Method != http.MethodPost {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body><h1>Fossil Sync Server</h1></body></html>")
			return
		}

		// Bound request body size to prevent resource exhaustion.
		req.Body = http.MaxBytesReader(w, req.Body, maxRequestBodyBytes)

		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}

		if len(body) == 0 {
			// Empty POST — respond with empty xfer message.
			writeXferResponse(w, &xfer.Message{})
			return
		}

		// §4: select framing from the request's Content-Type rather than by
		// trial. A canonical or libfossil client sends the compressed container
		// as application/x-fossil. A type we do not recognise carries no framing
		// signal at all, so decoding it either way is a guess -- reject it rather
		// than feed possibly-compressed bytes to the card parser (issue #104).
		ct := req.Header.Get("Content-Type")
		if !xfer.KnownContentType(ct) {
			slog.Error("serve-http: unsupported content type", "content-type", ct)
			http.Error(w, fmt.Sprintf("unsupported content type %q: expected %s or %s",
				ct, xfer.ContentTypeCompressed, xfer.ContentTypeUncompressed),
				http.StatusUnsupportedMediaType)
			return
		}

		msg, err := xfer.Decode(body, ct)
		if err != nil {
			slog.Error("serve-http: decode failed", "bytes", len(body), "err", err)
			http.Error(w, fmt.Sprintf("decode xfer (%d bytes): %v", len(body), err),
				http.StatusBadRequest)
			return
		}

		resp, err := h(req.Context(), r, msg)
		if err != nil {
			http.Error(w, "handler: "+err.Error(), http.StatusInternalServerError)
			return
		}

		writeXferResponse(w, resp)
	}
}

// writeXferResponse encodes an xfer message and writes it as the HTTP response.
func writeXferResponse(w http.ResponseWriter, msg *xfer.Message) {
	respBytes, err := msg.Encode()
	if err != nil {
		http.Error(w, "encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", xfer.ContentTypeCompressed)
	if _, err := w.Write(respBytes); err != nil {
		slog.Error("serve-http: write response failed", "err", err)
	}
}

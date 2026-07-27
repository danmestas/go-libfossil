package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/danmestas/go-libfossil/internal/xfer"
)

// postErr shapes an error the way http.Client reports one, since that wrapper
// is what tells the classifier the failure happened on the wire.
func postErr(cause error) error {
	return &url.Error{Op: "Post", URL: "http://peer.example/", Err: cause}
}

// TestRetryableExchangeFaultDiscriminates pins which failures earn another
// attempt. The two halves matter equally: retrying a severed connection is the
// point of #185, and NOT retrying a reply that arrived whole and would not
// decode is what keeps a reproducible defect from becoming an intermittent one.
func TestRetryableExchangeFaultDiscriminates(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},

		// Retryable: the exchange was cut off before any reply was read.
		{"post EOF", postErr(io.EOF), true},
		{"post unexpected EOF", postErr(io.ErrUnexpectedEOF), true},
		{"connection reset", postErr(&net.OpError{Op: "read", Err: syscall.ECONNRESET}), true},
		{"connection refused", postErr(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), true},
		{"bare errno", syscall.ECONNRESET, true},
		{"i/o timeout", postErr(&net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}), true},
		{"temporary DNS failure", postErr(&net.DNSError{Err: "server misbehaving", IsTemporary: true}), true},
		{"tagged read fault", RetryableFault(fmt.Errorf("read: %w", io.ErrUnexpectedEOF)), true},

		// Retryable: the peer said it could not serve this request now.
		{"503", &StatusError{Code: 503, Status: "503 Service Unavailable"}, true},
		{"500", &StatusError{Code: 500, Status: "500 Internal Server Error"}, true},
		{"429", &StatusError{Code: 429, Status: "429 Too Many Requests"}, true},
		{"408", &StatusError{Code: 408, Status: "408 Request Timeout"}, true},

		// Fatal: the peer rejected the request itself.
		{"400", &StatusError{Code: 400, Status: "400 Bad Request"}, false},
		{"401", &StatusError{Code: 401, Status: "401 Unauthorized"}, false},
		{"403", &StatusError{Code: 403, Status: "403 Forbidden"}, false},
		{"404", &StatusError{Code: 404, Status: "404 Not Found"}, false},

		// Fatal: a reply arrived whole and would not decode. This wraps the
		// same sentinel as a severed read, which is exactly why the wire
		// stages tag themselves rather than leaving it to be guessed.
		{"truncated body decode", fmt.Errorf("xfer: decompress: %w", io.ErrUnexpectedEOF), false},
		{"unparsable card", errors.New("xfer: pull requires 2 args, got 1"), false},

		// Fatal: no such host will not start existing.
		{"host not found", postErr(&net.DNSError{Err: "no such host", IsNotFound: true}), false},

		// Fatal: the caller's own decision, not a fault.
		{"cancelled", postErr(context.Canceled), false},
		{"deadline exceeded", postErr(context.DeadlineExceeded), false},

		// Fatal: an unrecognised error from a Transport this package does not
		// know. The simulated network in dst/ raises these, and its fault
		// injection must keep meaning what it meant.
		{"unknown transport error", errors.New("simnet: message from n1 dropped"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableExchangeFault(tc.err); got != tc.want {
				t.Errorf("retryableExchangeFault(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// countingTransport fails a fixed number of times before succeeding.
type countingTransport struct {
	failures int
	fail     error
	attempts int
}

func (t *countingTransport) Exchange(_ context.Context, _ *xfer.Message) (*xfer.Message, error) {
	t.attempts++
	if t.attempts <= t.failures {
		return nil, t.fail
	}
	return &xfer.Message{}, nil
}

func TestExchangeWithRetrySucceedsAfterTransientFailures(t *testing.T) {
	tr := &countingTransport{failures: 2, fail: postErr(io.EOF)}
	resp, err := exchangeWithRetry(context.Background(), tr, &xfer.Message{}, nil, nil)
	if err != nil {
		t.Fatalf("exchangeWithRetry: %v", err)
	}
	if resp == nil {
		t.Fatal("exchangeWithRetry returned a nil message with no error")
	}
	if tr.attempts != 3 {
		t.Errorf("transport saw %d attempts, want 3", tr.attempts)
	}
}

func TestExchangeWithRetryStopsAtBound(t *testing.T) {
	tr := &countingTransport{failures: MaxExchangeAttempts + 5, fail: postErr(io.EOF)}
	_, err := exchangeWithRetry(context.Background(), tr, &xfer.Message{}, nil, nil)
	if err == nil {
		t.Fatal("exchangeWithRetry returned nil error for a peer that never replies")
	}
	if tr.attempts != MaxExchangeAttempts {
		t.Errorf("transport saw %d attempts, want the bound of %d", tr.attempts, MaxExchangeAttempts)
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("exhausted error does not carry the underlying fault: %v", err)
	}
}

func TestExchangeWithRetryDoesNotRetryProtocolFaults(t *testing.T) {
	tr := &countingTransport{failures: 99, fail: errors.New("xfer: pull requires 2 args, got 1")}
	if _, err := exchangeWithRetry(context.Background(), tr, &xfer.Message{}, nil, nil); err == nil {
		t.Fatal("exchangeWithRetry returned nil error for an unparsable reply")
	}
	if tr.attempts != 1 {
		t.Errorf("transport saw %d attempts, want 1: a protocol fault must not be retried", tr.attempts)
	}
}

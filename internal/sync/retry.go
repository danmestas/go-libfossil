package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// Bounds on retrying a single exchange round. A clone of a large repository is
// a conversation of many rounds spanning minutes, so one transient drop
// anywhere in it used to discard the whole transfer (#185). Retrying is
// bounded so a peer that is genuinely gone still fails promptly: the delays
// below sum to under two seconds before the last attempt.
const (
	// MaxExchangeAttempts is how many times one round is sent before the
	// clone or sync gives up, the first attempt included.
	MaxExchangeAttempts = 4
	// ExchangeRetryBaseDelay is the wait before the second attempt. It
	// doubles before each further attempt, capped at ExchangeRetryMaxDelay.
	ExchangeRetryBaseDelay = 250 * time.Millisecond
	// ExchangeRetryMaxDelay caps the backoff between attempts.
	ExchangeRetryMaxDelay = 2 * time.Second
)

// TransportFault is implemented by an error that classifies itself for the
// retry decision. A Transport knows which stage of the exchange failed —
// whether bytes were still crossing the wire or a reply had already arrived
// and was being interpreted — and that distinction cannot be recovered
// reliably from the error's shape afterwards, since a truncated zlib body and
// a truncated wire read both surface as [io.ErrUnexpectedEOF].
//
// It is optional in the same way [FramedTransport] is: a Transport that does
// not implement it has its errors classified by inspection instead, which is
// deliberately conservative — anything not recognisably a network fault is
// treated as fatal.
type TransportFault interface {
	error
	// RetryableTransportFault reports whether sending the same request
	// again may succeed. It is true only when no reply was interpreted, so
	// a resend cannot mask a defect in one that was.
	RetryableTransportFault() bool
}

// transportFault tags an error with a retry verdict its raiser already knows.
type transportFault struct {
	err       error
	retryable bool
}

func (e *transportFault) Error() string                 { return e.err.Error() }
func (e *transportFault) Unwrap() error                 { return e.err }
func (e *transportFault) RetryableTransportFault() bool { return e.retryable }

// RetryableFault tags an error raised while bytes were still crossing the
// wire, so the round loop may resend the same request. Transports outside this
// package use it for their read stage, where a short read means the reply was
// cut off rather than malformed; a failure raised while interpreting a reply
// that arrived whole must never be tagged.
func RetryableFault(err error) error {
	if err == nil {
		panic("sync.RetryableFault: err must not be nil")
	}
	return &transportFault{err: err, retryable: true}
}

// StatusError reports a non-2xx HTTP reply from a sync peer. It is raised
// instead of decoding the body, because an error page is not an xfer reply and
// reporting it as a decode failure hides what actually happened.
type StatusError struct {
	Code   int    // HTTP status code
	Status string // HTTP status line, e.g. "503 Service Unavailable"
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("sync: peer replied %s", e.Status)
}

// RetryableTransportFault treats 5xx, 429 and 408 as worth another attempt:
// each says the peer could not serve this request now, not that the request
// was wrong. Every other status — 4xx above all — is the peer rejecting the
// request itself, which repeating cannot fix and would only obscure.
func (e *StatusError) RetryableTransportFault() bool {
	return e.Code >= 500 ||
		e.Code == http.StatusTooManyRequests ||
		e.Code == http.StatusRequestTimeout
}

// exchangeWithRetry runs one exchange round, resending it after a bounded
// exponential backoff when the failure was a transport fault rather than a
// protocol one. The caller's context governs throughout: a cancellation or
// deadline aborts immediately, including during a backoff wait, so retrying
// can never extend an operation past the bound its caller set.
func exchangeWithRetry(ctx context.Context, t Transport, req *xfer.Message, clock simio.Clock, obs Observer) (*xfer.Message, error) {
	if t == nil {
		panic("sync.exchangeWithRetry: t must not be nil")
	}
	if req == nil {
		panic("sync.exchangeWithRetry: req must not be nil")
	}
	if clock == nil {
		clock = simio.RealClock{}
	}
	obs = resolveObserver(obs)

	delay := ExchangeRetryBaseDelay
	for attempt := 1; ; attempt++ {
		resp, err := t.Exchange(ctx, req)
		if err == nil {
			return resp, nil
		}
		// A cancelled or expired context is the caller's decision, never a
		// fault to retry — and it is reported as itself so callers keep
		// matching on context.Canceled and context.DeadlineExceeded.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if !retryableExchangeFault(err) {
			return nil, err
		}
		if attempt >= MaxExchangeAttempts {
			return nil, fmt.Errorf("after %d attempts: %w", MaxExchangeAttempts, err)
		}

		obs.Error(ctx, fmt.Errorf("sync: exchange attempt %d of %d failed, retrying in %v: %w",
			attempt, MaxExchangeAttempts, delay, err))

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-clock.After(delay):
		}
		delay *= 2
		if delay > ExchangeRetryMaxDelay {
			delay = ExchangeRetryMaxDelay
		}
	}
}

// retryableExchangeFault reports whether an exchange failure is worth another
// attempt. It is deliberately an allow-list: only failures positively
// identified as the network dropping the exchange are retried, and everything
// else — a reply that would not decode, a card the peer rejected, an auth
// failure, an unrecognised error from a Transport this package does not know —
// fails on its first occurrence. Retrying those would turn a reproducible
// defect into an intermittent one, which is worse than the failure retrying
// exists to prevent.
func retryableExchangeFault(err error) bool {
	if err == nil {
		return false
	}
	// A context that was cancelled or ran out is the caller's decision, not a
	// fault. A transport that imposes a deadline of its own also surfaces as
	// context.DeadlineExceeded and is treated the same way here — the two are
	// indistinguishable once wrapped, and refusing to retry is the
	// conservative reading. A network-level i/o timeout, which is distinct
	// from either, is still retried below.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// A Transport that knows which stage failed has already said so.
	var fault TransportFault
	if errors.As(err, &fault) {
		return fault.RetryableTransportFault()
	}

	// An http.Client failure is always a *url.Error, and everything under it
	// was raised while the request or reply was still on the wire — no reply
	// has been interpreted at that point. This is where the reported failure
	// lands: "Post http://...: EOF", the peer going away mid-exchange.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return roundTripFaultIsTransient(urlErr.Err)
	}

	return networkFault(err)
}

// roundTripFaultIsTransient classifies a cause raised while an HTTP round trip
// was still in flight. A truncated or closed connection counts here — unlike
// in [networkFault], where the same sentinels could equally have come from a
// decoder reading a short body, which is a protocol fault.
func roundTripFaultIsTransient(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return networkFault(err)
}

// networkFault reports whether err is recognisably the network failing, judged
// only by types the net package raises for connection-level failures.
func networkFault(err error) bool {
	// Name resolution: a temporary failure is worth retrying, but a name
	// that does not exist will not start existing.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTemporary || dnsErr.IsTimeout
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Reached when a connection error arrives without its *net.OpError
	// wrapper, e.g. from a Transport that unwrapped it.
	for _, errno := range []syscall.Errno{
		syscall.ECONNRESET,
		syscall.ECONNABORTED,
		syscall.ECONNREFUSED,
		syscall.EPIPE,
		syscall.EHOSTUNREACH,
		syscall.ENETUNREACH,
		syscall.ENETDOWN,
		syscall.ETIMEDOUT,
	} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

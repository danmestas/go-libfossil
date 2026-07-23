package cli

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCloneContextZeroTimeoutDisablesDeadline pins the branch the --timeout 0
// flag exists for. The naive spelling -- always context.WithTimeout(parent,
// timeout) -- turns 0 into a deadline that has *already* passed, so the clone it
// was meant to unbound would abort instantly instead. Asserting the flag parses
// to 0 cannot catch that; only checking the context it produces can.
func TestCloneContextZeroTimeoutDisablesDeadline(t *testing.T) {
	ctx, cancel := cloneContext(context.Background(), 0)
	defer cancel()

	if deadline, ok := ctx.Deadline(); ok {
		t.Fatalf("timeout 0 produced a context with deadline %v, want none", deadline)
	}

	// Long enough that a zero-duration WithTimeout would certainly have fired.
	time.Sleep(50 * time.Millisecond)

	select {
	case <-ctx.Done():
		t.Fatalf("timeout 0 context is already done (%v); the deadline was not disabled", ctx.Err())
	default:
	}
}

// TestCloneContextNegativeTimeoutDisablesDeadline covers the other side of the
// "not positive" guard: a negative duration is treated as disabled rather than
// as an instantly-expired deadline.
func TestCloneContextNegativeTimeoutDisablesDeadline(t *testing.T) {
	ctx, cancel := cloneContext(context.Background(), -time.Second)
	defer cancel()

	if _, ok := ctx.Deadline(); ok {
		t.Fatal("negative timeout produced a deadline, want none")
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("negative timeout context is already done: %v", err)
	}
}

// TestCloneContextPositiveTimeoutSetsDeadline pins the ordinary case: a positive
// timeout really does bound the clone, and it fires.
func TestCloneContextPositiveTimeoutSetsDeadline(t *testing.T) {
	ctx, cancel := cloneContext(context.Background(), 30*time.Millisecond)
	defer cancel()

	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("positive timeout produced no deadline")
	}

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Errorf("ctx.Err() = %v, want context.DeadlineExceeded", ctx.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("positive timeout never fired")
	}
}

// TestCloneContextCancelReleasesContext keeps the returned cancel meaningful in
// the disabled case, so callers get uniform cleanup either way.
func TestCloneContextCancelReleasesContext(t *testing.T) {
	ctx, cancel := cloneContext(context.Background(), 0)
	cancel()

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release the timeout-disabled context")
	}
}

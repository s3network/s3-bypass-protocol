package signal

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// CancelAfterInactivity must honour its ctx: a long inactivity timeout used to
// leave the periodic check task running (and its goroutine parked) even after
// the owning context was already done.
func TestCancelAfterInactivityStopsWhenContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	timer := CancelAfterInactivity(ctx, cancel, time.Hour)

	assert.False(t, timer.consumed.Load())
	cancel()

	// The timer must finish on its own now, without waiting for the timeout.
	assert.Eventually(t, timer.consumed.Load, 5*time.Second, 20*time.Millisecond,
		"timer kept running after its context was done")

	// A later SetTimeout must not resurrect the consumed timer.
	timer.SetTimeout(time.Second)
	assert.True(t, timer.consumed.Load())

	// Finishing twice must stay a no-op.
	assert.NotPanics(t, func() { timer.finish() })
}

// A background context has no Done channel; the timer must not block forever
// on it.
func TestCancelAfterInactivityWithBackgroundContext(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	timer := CancelAfterInactivity(context.Background(), cancel, 0)
	assert.True(t, timer.consumed.Load())
}

// Cancelling the context must not fire the cancel callback a second time: the
// callback belongs to the inactivity path, and callers rely on it being
// idempotent, not on it being invoked twice.
func TestCancelAfterInactivityCancelIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	timer := CancelAfterInactivity(ctx, cancel, time.Hour)

	cancel()
	assert.Eventually(t, timer.consumed.Load, 5*time.Second, 20*time.Millisecond)

	// Calling the same cancel again stays safe.
	cancel()
	cancel()
}

package retry // import "github.com/xtls/xray-core/common/retry"

import (
	"context"
	"time"

	"github.com/xtls/xray-core/common/errors"
)

var ErrRetryFailed = errors.New("all retry attempts failed")

// Strategy is a way to retry on a specific function.
type Strategy interface {
	// On performs a retry on a specific function, until it doesn't return any error.
	// The retries are not cancellable, use OnWithContext to be able to stop them.
	On(func() error) error
	// OnWithContext behaves like On, but gives up as soon as ctx is done.
	OnWithContext(ctx context.Context, method func() error) error
}

type retryer struct {
	totalAttempt int
	nextDelay    func() uint32
}

// On implements Strategy.On.
func (r *retryer) On(method func() error) error {
	return r.OnWithContext(context.Background(), method)
}

// OnWithContext implements Strategy.OnWithContext. The backoff sleep is
// interruptible, so a shutdown cancels pending retries instead of waiting them
// out.
func (r *retryer) OnWithContext(ctx context.Context, method func() error) error {
	attempt := 0
	accumulatedError := make([]error, 0, r.totalAttempt)
	for attempt < r.totalAttempt {
		if err := ctx.Err(); err != nil {
			accumulatedError = append(accumulatedError, err)
			return errors.New(accumulatedError).Base(ErrRetryFailed)
		}

		err := method()
		if err == nil {
			return nil
		}
		numErrors := len(accumulatedError)
		if numErrors == 0 || err.Error() != accumulatedError[numErrors-1].Error() {
			accumulatedError = append(accumulatedError, err)
		}
		delay := r.nextDelay()
		timer := time.NewTimer(time.Duration(delay) * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			accumulatedError = append(accumulatedError, ctx.Err())
			return errors.New(accumulatedError).Base(ErrRetryFailed)
		}
		attempt++
	}
	return errors.New(accumulatedError).Base(ErrRetryFailed)
}

// Timed returns a retry strategy with fixed interval.
func Timed(attempts int, delay uint32) Strategy {
	return &retryer{
		totalAttempt: attempts,
		nextDelay: func() uint32 {
			return delay
		},
	}
}

func ExponentialBackoff(attempts int, delay uint32) Strategy {
	nextDelay := uint32(0)
	return &retryer{
		totalAttempt: attempts,
		nextDelay: func() uint32 {
			r := nextDelay
			nextDelay += delay
			return r
		},
	}
}

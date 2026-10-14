package retry_test

import (
	"context"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/errors"
	. "github.com/xtls/xray-core/common/retry"
)

var errorTestOnly = errors.New("this is a fake error")

func TestNoRetry(t *testing.T) {
	startTime := time.Now().Unix()
	err := Timed(10, 100000).On(func() error {
		return nil
	})
	endTime := time.Now().Unix()

	common.Must(err)
	if endTime < startTime {
		t.Error("endTime < startTime: ", startTime, " -> ", endTime)
	}
}

func TestRetryOnce(t *testing.T) {
	startTime := time.Now()
	called := 0
	err := Timed(10, 1000).On(func() error {
		if called == 0 {
			called++
			return errorTestOnly
		}
		return nil
	})
	duration := time.Since(startTime)

	common.Must(err)
	if v := int64(duration / time.Millisecond); v < 900 {
		t.Error("duration: ", v)
	}
}

func TestRetryMultiple(t *testing.T) {
	startTime := time.Now()
	called := 0
	err := Timed(10, 1000).On(func() error {
		if called < 5 {
			called++
			return errorTestOnly
		}
		return nil
	})
	duration := time.Since(startTime)

	common.Must(err)
	if v := int64(duration / time.Millisecond); v < 4900 {
		t.Error("duration: ", v)
	}
}

func TestRetryExhausted(t *testing.T) {
	startTime := time.Now()
	called := 0
	err := Timed(2, 1000).On(func() error {
		called++
		return errorTestOnly
	})
	duration := time.Since(startTime)

	if errors.Cause(err) != ErrRetryFailed {
		t.Error("cause: ", err)
	}

	if v := int64(duration / time.Millisecond); v < 1900 {
		t.Error("duration: ", v)
	}
}

func TestExponentialBackoff(t *testing.T) {
	startTime := time.Now()
	called := 0
	err := ExponentialBackoff(10, 100).On(func() error {
		called++
		return errorTestOnly
	})
	duration := time.Since(startTime)

	if errors.Cause(err) != ErrRetryFailed {
		t.Error("cause: ", err)
	}
	if v := int64(duration / time.Millisecond); v < 4000 {
		t.Error("duration: ", v)
	}
}

func TestRetryCancelledByContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := 0
	err := ExponentialBackoff(10, 1000).OnWithContext(ctx, func() error {
		called++
		return errorTestOnly
	})

	if called != 0 {
		t.Fatal("expected no attempt on an already cancelled context, got ", called)
	}
	if errors.Cause(err) != ErrRetryFailed {
		t.Error("cause: ", err)
	}
}

func TestRetryCancelledWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startTime := time.Now()
	called := 0
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	// The backoff of this strategy is 10s, so only the cancellation can end it.
	err := Timed(10, 10000).OnWithContext(ctx, func() error {
		called++
		return errorTestOnly
	})
	duration := time.Since(startTime)

	if called != 1 {
		t.Fatal("expected exactly one attempt, got ", called)
	}
	if errors.Cause(err) != ErrRetryFailed {
		t.Error("cause: ", err)
	}
	if duration > 5*time.Second {
		t.Fatal("cancellation did not interrupt the backoff, took ", duration)
	}
}

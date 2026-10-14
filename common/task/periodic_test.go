package task_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/errors"
	. "github.com/xtls/xray-core/common/task"
)

// waitExecutions waits for n executions to be signalled, or fails the test.
func waitExecutions(t *testing.T, executed chan struct{}, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-executed:
		case <-deadline:
			t.Fatalf("timed out after %d of %d executions", i, n)
		}
	}
}

// drainExecutions drops the signals that arrived so far.
func drainExecutions(executed chan struct{}) {
	for {
		select {
		case <-executed:
		default:
			return
		}
	}
}

// The task goroutine and the test must not share an unsynchronized variable, and
// the assertions must not depend on how many intervals happened to fit into a
// sleep. The counter is atomic and every wait is bounded by a signal.
func TestPeriodicTaskStop(t *testing.T) {
	const interval = 100 * time.Millisecond

	var value atomic.Int32
	executed := make(chan struct{}, 64)

	task := &Periodic{
		Interval: interval,
		Execute: func() error {
			value.Add(1)
			select {
			case executed <- struct{}{}:
			default:
			}
			return nil
		},
	}
	common.Must(task.Start())

	// Start() runs Execute synchronously once.
	if v := value.Load(); v != 1 {
		t.Fatalf("expected 1 execution right after Start, got %d", v)
	}
	drainExecutions(executed)

	waitExecutions(t, executed, 2)
	if v := value.Load(); v < 3 {
		t.Fatalf("expected at least 3 executions, got %d", v)
	}

	common.Must(task.Close())

	// Let an execution that was already in flight finish before sampling.
	time.Sleep(2 * interval)
	stopped := value.Load()
	drainExecutions(executed)

	time.Sleep(5 * interval)
	if v := value.Load(); v != stopped {
		t.Fatalf("expected the task to stay at %d executions, but it reached %d", stopped, v)
	}

	common.Must(task.Start())
	waitExecutions(t, executed, 2)
	if v := value.Load(); v <= stopped {
		t.Fatalf("expected the task to run again after Start, still at %d", v)
	}

	common.Must(task.Close())
}

func TestPeriodicTaskStopsOnExecuteError(t *testing.T) {
	const interval = 100 * time.Millisecond

	var calls atomic.Int32
	task := &Periodic{
		Interval: interval,
		Execute: func() error {
			calls.Add(1)
			return errPeriodicStop
		},
	}
	if err := task.Start(); err == nil {
		t.Fatal("expected Start to report the error of the first execution")
	}

	// A failing task must not keep rescheduling itself.
	time.Sleep(5 * interval)
	if v := calls.Load(); v != 1 {
		t.Fatalf("expected the task to give up after 1 call, got %d", v)
	}
	common.Must(task.Close())
}

var errPeriodicStop = errors.New("stop the periodic task")

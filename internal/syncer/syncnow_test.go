package syncer

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/r1chjames/sftp-sync/internal/config"
)

// testSyncer builds a started-ready syncer whose manifest lives in a temp
// directory, so Start never touches the real data directory.
func testSyncer(t *testing.T, interval time.Duration) *Syncer {
	t.Helper()
	s := New(&config.Config{
		StatePath: filepath.Join(t.TempDir(), "manifest.json"),
		Sync:      config.SyncConfig{Interval: interval, Workers: 1},
	})
	return s
}

func waitForSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func assertNoSignal(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("unexpected %s", what)
	case <-time.After(d):
	}
}

func TestStartRunsCycleAndSyncNowTriggersAnother(t *testing.T) {
	s := testSyncer(t, time.Hour)
	entered := make(chan struct{}, 4)
	s.syncFn = func(context.Context) error {
		entered <- struct{}{}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForSignal(t, entered, "startup cycle")

	// The interval is an hour, so only SyncNow can produce another cycle.
	s.SyncNow()
	waitForSignal(t, entered, "cycle triggered by SyncNow")

	assertNoSignal(t, entered, 200*time.Millisecond, "cycle without a request")

	s.Stop()
}

func TestSyncNowDuringCycleCoalescesIntoOneFollowUp(t *testing.T) {
	s := testSyncer(t, time.Hour)
	entered := make(chan struct{}, 64)
	release := make(chan struct{})
	s.syncFn = func(context.Context) error {
		entered <- struct{}{}
		<-release
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForSignal(t, entered, "first cycle")

	// Ten requests while a cycle is running must collapse into one follow-up.
	for i := 0; i < 10; i++ {
		s.SyncNow()
	}
	release <- struct{}{}

	waitForSignal(t, entered, "coalesced follow-up cycle")
	release <- struct{}{}

	assertNoSignal(t, entered, 300*time.Millisecond, "second follow-up cycle")

	cancel()
	s.Stop()
}

func TestCyclesNeverOverlap(t *testing.T) {
	s := testSyncer(t, 5*time.Millisecond)

	var concurrent, overlapped, cycles int32
	s.syncFn = func(context.Context) error {
		if atomic.AddInt32(&concurrent, 1) > 1 {
			atomic.StoreInt32(&overlapped, 1)
		}
		time.Sleep(2 * time.Millisecond)
		atomic.AddInt32(&concurrent, -1)
		atomic.AddInt32(&cycles, 1)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		s.SyncNow()
		time.Sleep(time.Millisecond)
	}
	s.Stop()

	if atomic.LoadInt32(&overlapped) != 0 {
		t.Fatal("two sync cycles ran concurrently for the same job")
	}
	if got := atomic.LoadInt32(&cycles); got < 2 {
		t.Fatalf("cycles = %d, want at least 2 (interval and sync-now)", got)
	}
}

func TestIntervalTriggersRepeatedCycles(t *testing.T) {
	s := testSyncer(t, 10*time.Millisecond)
	var cycles int32
	entered := make(chan struct{}, 128)
	s.syncFn = func(context.Context) error {
		atomic.AddInt32(&cycles, 1)
		select {
		case entered <- struct{}{}:
		default:
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&cycles) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s.Stop()

	if got := atomic.LoadInt32(&cycles); got < 3 {
		t.Fatalf("cycles = %d, want at least 3 from the interval alone", got)
	}
}

func TestStopInterruptsIdleWait(t *testing.T) {
	s := testSyncer(t, time.Hour)
	entered := make(chan struct{}, 2)
	s.syncFn = func(context.Context) error {
		entered <- struct{}{}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForSignal(t, entered, "startup cycle")

	// The loop is now waiting an hour; Stop must not wait for it.
	stopped := make(chan struct{})
	go func() {
		s.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop blocked while the loop waited for its interval")
	}
}

func TestSyncNowDoesNotBlockWithoutListener(t *testing.T) {
	s := testSyncer(t, time.Hour)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			s.SyncNow()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("SyncNow blocked with no cycle running")
	}
}

func TestSyncNowBeforeStartDoesNotPanic(t *testing.T) {
	s := testSyncer(t, time.Hour)
	s.SyncNow()
	s.SyncNow()
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Stop()
}

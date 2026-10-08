package syncer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func waitForPhase(t *testing.T, s *Syncer, want SyncPhase) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Status().Phase == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("phase = %q, want %q", s.Status().Phase, want)
}

func TestPauseWhileIdlePublishesPausedPhase(t *testing.T) {
	s := testSyncer(t, time.Hour)
	entered := make(chan struct{}, 8)
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

	s.Pause()
	waitForPhase(t, s, PhasePaused)
	if !s.IsPaused() {
		t.Fatal("IsPaused = false after Pause")
	}

	// A paused job must not start further cycles even though its interval is
	// an hour and pause interrupted the wait.
	assertNoSignal(t, entered, 200*time.Millisecond, "cycle while paused")

	s.Stop()
}

func TestPauseDuringCycleTakesEffectWhenCycleEnds(t *testing.T) {
	s := testSyncer(t, time.Hour)
	entered := make(chan struct{}, 8)
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

	s.Pause()

	// The loop is inside the cycle, so the phase stays as it was until the
	// cycle finishes.
	if got := s.Status().Phase; got == PhasePaused {
		t.Fatalf("phase = %q while a cycle is still running", got)
	}

	release <- struct{}{}
	waitForPhase(t, s, PhasePaused)
	assertNoSignal(t, entered, 200*time.Millisecond, "cycle after a mid-cycle pause")

	cancel()
	s.Stop()
}

func TestResumeTriggersImmediateScan(t *testing.T) {
	s := testSyncer(t, time.Hour)
	entered := make(chan struct{}, 8)
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

	s.Pause()
	waitForPhase(t, s, PhasePaused)
	assertNoSignal(t, entered, 200*time.Millisecond, "cycle while paused")

	s.Resume()
	if s.IsPaused() {
		t.Fatal("IsPaused = true after Resume")
	}
	waitForSignal(t, entered, "cycle immediately after resume")

	s.Stop()
}

func TestPauseAndResumeAreIdempotent(t *testing.T) {
	s := testSyncer(t, time.Hour)
	entered := make(chan struct{}, 8)
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

	s.Pause()
	s.Pause()
	waitForPhase(t, s, PhasePaused)

	s.Resume()
	s.Resume()
	waitForSignal(t, entered, "cycle after resume")

	// The duplicate resume must not queue a second follow-up cycle.
	assertNoSignal(t, entered, 300*time.Millisecond, "extra cycle from a duplicate resume")

	s.Stop()
}

func TestSyncNowWhilePausedRunsAfterResume(t *testing.T) {
	s := testSyncer(t, time.Hour)
	entered := make(chan struct{}, 8)
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

	s.Pause()
	waitForPhase(t, s, PhasePaused)

	s.SyncNow()
	s.SyncNow()
	assertNoSignal(t, entered, 200*time.Millisecond, "cycle while paused")

	s.Resume()
	waitForSignal(t, entered, "cycle after resume")
	assertNoSignal(t, entered, 300*time.Millisecond, "more than one cycle from buffered requests")

	s.Stop()
}

func TestStartWhilePausedRunsNoCycle(t *testing.T) {
	s := testSyncer(t, time.Hour)
	var cycles int32
	s.syncFn = func(context.Context) error {
		atomic.AddInt32(&cycles, 1)
		return nil
	}

	// A restored paused job is paused before Start, so the loop's first gate
	// check already sees the paused state and never opens a connection.
	s.Pause()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForPhase(t, s, PhasePaused)
	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt32(&cycles); got != 0 {
		t.Fatalf("cycles = %d, want 0 while restored paused", got)
	}
	if st := s.Status(); !st.Paused || st.Phase != PhasePaused {
		t.Fatalf("status = %+v, want paused", st)
	}

	s.Stop()
}

func TestPausedFlagIsPublishedInStatus(t *testing.T) {
	s := testSyncer(t, time.Hour)
	if s.Status().Paused {
		t.Fatal("a new syncer must not report paused")
	}

	s.Pause()
	if !s.Status().Paused {
		t.Fatal("status.Paused = false after Pause")
	}

	s.Resume()
	if s.Status().Paused {
		t.Fatal("status.Paused = true after Resume")
	}
}

func TestStopWhilePaused(t *testing.T) {
	s := testSyncer(t, time.Hour)
	entered := make(chan struct{}, 8)
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
	s.Pause()
	waitForPhase(t, s, PhasePaused)

	stopped := make(chan struct{})
	go func() {
		s.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop blocked while paused")
	}
}

func TestStopWhileCycleRunning(t *testing.T) {
	s := testSyncer(t, time.Hour)
	entered := make(chan struct{}, 8)
	s.syncFn = func(ctx context.Context) error {
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForSignal(t, entered, "startup cycle")

	stopped := make(chan struct{})
	go func() {
		s.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop blocked while a cycle was running")
	}

	// A shutdown-cancelled cycle is not a failure.
	if st := s.Status(); st.LastError != nil {
		t.Fatalf("shutdown recorded an error: %v", st.LastError)
	}
}

func TestShouldStopSubmitting(t *testing.T) {
	s := testSyncer(t, time.Hour)
	live := context.Background()

	if s.shouldStopSubmitting(live) {
		t.Fatal("an active syncer must keep submitting files")
	}

	s.Pause()
	if !s.shouldStopSubmitting(live) {
		t.Fatal("a paused syncer must stop submitting files")
	}

	s.Resume()
	if s.shouldStopSubmitting(live) {
		t.Fatal("a resumed syncer must submit files again")
	}

	cancelled, cancel := context.WithCancel(live)
	cancel()
	if !s.shouldStopSubmitting(cancelled) {
		t.Fatal("a cancelled context must stop submitting files")
	}
}

func TestRecordCycleTreatsPausedBatchAsNeitherSuccessNorFailure(t *testing.T) {
	s := testSyncer(t, time.Hour)
	s.markSuccess()
	lastSuccess := s.Status().LastSuccessfulSync

	s.beginDownload(3, 100)
	s.recordBytes("/photos/a.jpg", 40, 40, 40)
	s.recordFileResult("/photos/a.jpg", nil, 40)

	s.recordCycle(context.Background(), errBatchPaused)

	st := s.Status()
	if st.LastError != nil {
		t.Fatalf("paused batch recorded an error: %v", st.LastError)
	}
	if !st.LastSuccessfulSync.Equal(lastSuccess) {
		t.Fatalf("paused batch advanced last successful sync to %v", st.LastSuccessfulSync)
	}
	if st.Completed != 1 || st.Remaining != 2 || st.BytesCompleted != 40 {
		t.Fatalf("counters should be preserved: %+v", st)
	}
}

func TestMarkPausedClearsInFlightFile(t *testing.T) {
	s := testSyncer(t, time.Hour)
	s.beginDownload(1, 50)
	s.setCurrentFile("/photos/a.jpg")
	s.recordBytes("/photos/a.jpg", 20, 20, 50)

	s.markPaused()

	st := s.Status()
	if st.Phase != PhasePaused {
		t.Fatalf("phase = %q, want %q", st.Phase, PhasePaused)
	}
	if st.CurrentFile != "" || st.CurrentFileBytesTotal != 0 || st.CurrentFileBytesCompleted != 0 {
		t.Fatalf("in-flight file fields not cleared: %+v", st)
	}
	if st.BytesTotal != 50 || st.BytesCompleted != 20 {
		t.Fatalf("batch byte totals must be preserved: %+v", st)
	}
}

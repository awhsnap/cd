package jobs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"calendardynamics/internal/db"
	"calendardynamics/internal/jobs"
)

func setup(t *testing.T) *jobs.Queue {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return jobs.New(d, slog.New(slog.NewTextHandler(io.Discard, nil)), map[string]int{"default": 2})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting")
}

func TestRunsJob(t *testing.T) {
	q := setup(t)
	var ran atomic.Int32
	q.Register("hello", "default", 3, func(ctx context.Context, j jobs.Job) error { ran.Add(1); return nil })
	ctx := context.Background()
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer q.Stop(ctx)
	if err := q.Enqueue(ctx, "hello", map[string]string{"a": "b"}, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return ran.Load() == 1 })
}

func TestUniqueKeyUpdatesInsteadOfDuplicating(t *testing.T) {
	q := setup(t)
	var ran atomic.Int32
	q.Register("once", "default", 3, func(ctx context.Context, j jobs.Job) error { ran.Add(1); return nil })
	ctx := context.Background()
	soon := time.Now().Add(300 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if err := q.Enqueue(ctx, "once", i, soon, "followup:evt_1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer q.Stop(ctx)
	waitFor(t, func() bool { return ran.Load() >= 1 })
	time.Sleep(1500 * time.Millisecond)
	if n := ran.Load(); n != 1 {
		t.Fatalf("unique job ran %d times", n)
	}
}

func TestRetryThenSucceed(t *testing.T) {
	q := setup(t)
	var calls atomic.Int32
	q.Register("flaky", "default", 5, func(ctx context.Context, j jobs.Job) error {
		if calls.Add(1) == 1 {
			// Retry immediately for the test by rescheduling instead of waiting out backoff.
			return jobs.Reschedule{At: time.Now()}
		}
		return nil
	})
	ctx := context.Background()
	_ = q.Start(ctx)
	defer q.Stop(ctx)
	_ = q.Enqueue(ctx, "flaky", nil, time.Time{}, "")
	waitFor(t, func() bool { return calls.Load() == 2 })
}

func TestFailureGivesUpAfterMaxAttempts(t *testing.T) {
	q := setup(t)
	var calls atomic.Int32
	q.Register("broken", "default", 1, func(ctx context.Context, j jobs.Job) error {
		calls.Add(1)
		if !j.Final() {
			t.Error("expected final attempt")
		}
		return errors.New("boom")
	})
	ctx := context.Background()
	_ = q.Start(ctx)
	defer q.Stop(ctx)
	_ = q.Enqueue(ctx, "broken", nil, time.Time{}, "")
	waitFor(t, func() bool {
		s, _ := q.Stats(ctx)
		return s.Failed == 1
	})
	if calls.Load() != 1 {
		t.Fatalf("ran %d times", calls.Load())
	}
}

func TestBackoff(t *testing.T) {
	if jobs.Backoff(1) != 15*time.Second || jobs.Backoff(3) != time.Minute || jobs.Backoff(20) != time.Hour {
		t.Fatal("unexpected backoff schedule")
	}
}

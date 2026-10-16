// Package jobs is a small durable job queue stored in the app's SQLite database.
//
// Jobs survive restarts and deploys (they're rows), retry with exponential
// backoff, run in named queues with their own concurrency, and can be made
// unique so the same follow-up is never queued twice. It runs inside the web
// process, so there is no separate worker machine or queue service to pay for.
//
// When the app moves to Postgres, this can be swapped for River (which has
// the same concepts) without changing job handlers much.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"calendardynamics/internal/db"
)

type Job struct {
	ID          int64
	Kind        string
	Args        json.RawMessage
	Attempts    int // includes the current attempt
	MaxAttempts int
}

// Final reports whether a failure now would be the job's last attempt.
func (j Job) Final() bool { return j.Attempts >= j.MaxAttempts }

type Handler func(ctx context.Context, job Job) error

// Reschedule is returned by a handler to run the job again later without
// counting it as a failed attempt (e.g. quiet hours, or the event moved).
type Reschedule struct{ At time.Time }

func (r Reschedule) Error() string { return "reschedule to " + r.At.Format(time.RFC3339) }

// Cancel is returned by a handler when the job should stop without retrying.
type Cancel struct{ Reason string }

func (c Cancel) Error() string { return "cancelled: " + c.Reason }

type kindInfo struct {
	queue       string
	handler     Handler
	maxAttempts int
}

type Queue struct {
	db       *sql.DB
	log      *slog.Logger
	queues   map[string]int // queue name -> worker count
	kinds    map[string]kindInfo
	poll     time.Duration
	wake     chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
	cancel   context.CancelFunc
}

func New(database *sql.DB, log *slog.Logger, queues map[string]int) *Queue {
	return &Queue{
		db:     database,
		log:    log,
		queues: queues,
		kinds:  map[string]kindInfo{},
		poll:   time.Second,
		wake:   make(chan struct{}, 1),
	}
}

// Register maps a job kind to its queue and handler.
func (q *Queue) Register(kind, queue string, maxAttempts int, h Handler) {
	if _, ok := q.queues[queue]; !ok {
		panic("jobs: unknown queue " + queue)
	}
	q.kinds[kind] = kindInfo{queue: queue, handler: h, maxAttempts: maxAttempts}
}

// Enqueue adds a job. If uniqueKey is set and an active job with that key
// exists, its run time and arguments are updated instead of adding a duplicate.
func (q *Queue) Enqueue(ctx context.Context, kind string, args any, runAt time.Time, uniqueKey string) error {
	info, ok := q.kinds[kind]
	if !ok {
		return fmt.Errorf("jobs: unknown kind %q", kind)
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return err
	}
	if runAt.IsZero() {
		runAt = time.Now()
	}
	var key any
	if uniqueKey != "" {
		key = uniqueKey
		res, err := q.db.ExecContext(ctx, `UPDATE jobs SET run_at = ?, args = ? WHERE unique_key = ? AND state = 'available'`,
			db.FormatTime(runAt), string(payload), uniqueKey)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			q.nudge()
			return nil
		}
	}
	_, err = q.db.ExecContext(ctx, `INSERT INTO jobs (queue, kind, args, run_at, max_attempts, unique_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		info.queue, kind, string(payload), db.FormatTime(runAt), info.maxAttempts, key, db.Now())
	if err == nil {
		q.nudge()
	}
	return err
}

func (q *Queue) nudge() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Start launches workers. Jobs left 'running' by a previous process (crash or
// restart mid-job) are made available again — this is a single-machine app,
// so nothing else could be running them.
func (q *Queue) Start(ctx context.Context) error {
	if _, err := q.db.ExecContext(ctx, `UPDATE jobs SET state = 'available' WHERE state = 'running'`); err != nil {
		return err
	}
	ctx, q.cancel = context.WithCancel(ctx)
	for name, n := range q.queues {
		for i := 0; i < n; i++ {
			q.wg.Add(1)
			go q.worker(ctx, name)
		}
	}
	q.wg.Add(1)
	go q.janitor(ctx)
	return nil
}

// Stop stops claiming new jobs and waits (up to the context deadline) for
// running jobs to finish. Unfinished jobs are picked up again on next start.
func (q *Queue) Stop(ctx context.Context) error {
	q.stopOnce.Do(func() {
		if q.cancel != nil {
			q.cancel()
		}
	})
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *Queue) worker(ctx context.Context, queue string) {
	defer q.wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		job, ok, err := q.claim(ctx, queue)
		if err != nil {
			if ctx.Err() == nil {
				q.log.Error("jobs: claim failed", "queue", queue, "err", err)
			}
		}
		if ok {
			q.run(job)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-time.After(q.poll):
		}
	}
}

func (q *Queue) claim(ctx context.Context, queue string) (Job, bool, error) {
	var j Job
	var args string
	err := q.db.QueryRowContext(ctx, `UPDATE jobs SET state = 'running', attempts = attempts + 1
		WHERE id = (SELECT id FROM jobs WHERE queue = ? AND state = 'available' AND run_at <= ? ORDER BY run_at, id LIMIT 1)
		RETURNING id, kind, args, attempts, max_attempts`, queue, db.Now()).
		Scan(&j.ID, &j.Kind, &args, &j.Attempts, &j.MaxAttempts)
	if errors.Is(err, sql.ErrNoRows) {
		return j, false, nil
	}
	if err != nil {
		return j, false, err
	}
	j.Args = json.RawMessage(args)
	return j, true, nil
}

// run executes a job. It deliberately uses a fresh context (not the worker's)
// so a shutdown lets the in-flight job finish rather than aborting mid-send.
func (q *Queue) run(j Job) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	info, ok := q.kinds[j.Kind]
	var err error
	if !ok {
		err = Cancel{Reason: "no handler registered for " + j.Kind}
	} else {
		err = safeCall(ctx, info.handler, j)
	}

	var res sql.Result
	var dbErr error
	var resched Reschedule
	var cancelled Cancel
	switch {
	case err == nil:
		res, dbErr = q.db.Exec(`UPDATE jobs SET state = 'completed', finished_at = ?, last_error = '' WHERE id = ?`, db.Now(), j.ID)
	case errors.As(err, &resched):
		res, dbErr = q.db.Exec(`UPDATE jobs SET state = 'available', run_at = ?, attempts = attempts - 1 WHERE id = ?`,
			db.FormatTime(resched.At), j.ID)
	case errors.As(err, &cancelled):
		res, dbErr = q.db.Exec(`UPDATE jobs SET state = 'cancelled', finished_at = ?, last_error = ? WHERE id = ?`,
			db.Now(), trim(cancelled.Reason), j.ID)
	case j.Final():
		q.log.Error("jobs: giving up", "kind", j.Kind, "id", j.ID, "attempts", j.Attempts, "err", err)
		res, dbErr = q.db.Exec(`UPDATE jobs SET state = 'failed', finished_at = ?, last_error = ? WHERE id = ?`,
			db.Now(), trim(err.Error()), j.ID)
	default:
		retryAt := time.Now().Add(Backoff(j.Attempts))
		q.log.Warn("jobs: will retry", "kind", j.Kind, "id", j.ID, "attempt", j.Attempts, "retry_at", retryAt, "err", err)
		res, dbErr = q.db.Exec(`UPDATE jobs SET state = 'available', run_at = ?, last_error = ? WHERE id = ?`,
			db.FormatTime(retryAt), trim(err.Error()), j.ID)
	}
	_ = res
	if dbErr != nil {
		q.log.Error("jobs: could not record result", "id", j.ID, "err", dbErr)
	}
}

func safeCall(ctx context.Context, h Handler, j Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(ctx, j)
}

// Backoff: 15s, 30s, 1m, 2m, 4m... capped at 1 hour.
func Backoff(attempt int) time.Duration {
	d := 15 * time.Second
	for i := 1; i < attempt && d < time.Hour; i++ {
		d *= 2
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// janitor deletes finished jobs after two weeks so the table stays small.
func (q *Queue) janitor(ctx context.Context) {
	defer q.wg.Done()
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		cutoff := db.FormatTime(time.Now().Add(-14 * 24 * time.Hour))
		if _, err := q.db.ExecContext(ctx, `DELETE FROM jobs WHERE state IN ('completed', 'cancelled', 'failed') AND finished_at < ?`, cutoff); err != nil && ctx.Err() == nil {
			q.log.Warn("jobs: cleanup failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Stats summarises the queue for the admin panel.
type Stats struct {
	Available, Running, Failed int
	RecentFailures             []Failure
}

type Failure struct {
	Kind, Error, At string
}

func (q *Queue) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := q.db.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE state = 'available'),
		count(*) FILTER (WHERE state = 'running'),
		count(*) FILTER (WHERE state = 'failed')
		FROM jobs`).Scan(&s.Available, &s.Running, &s.Failed)
	if err != nil {
		return s, err
	}
	rows, err := q.db.QueryContext(ctx, `SELECT kind, last_error, coalesce(finished_at, '') FROM jobs
		WHERE state = 'failed' ORDER BY id DESC LIMIT 10`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var f Failure
		if err := rows.Scan(&f.Kind, &f.Error, &f.At); err != nil {
			return s, err
		}
		s.RecentFailures = append(s.RecentFailures, f)
	}
	return s, rows.Err()
}

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 1000 {
		s = s[:1000]
	}
	return s
}

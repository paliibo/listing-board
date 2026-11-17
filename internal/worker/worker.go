// Package worker walks running sheets through their checklist.
//
// All progress lives in the database: which steps are done, and when the last
// one was saved. The worker keeps nothing in memory between rounds, so a
// process killed at any moment is replaced by one that carries on from the
// first pending step, with the same run id, after the same pause.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"listingboard/internal/board"
)

// maxWait bounds how long the worker sleeps, so that a sheet started while it
// waits for another one is noticed quickly.
const maxWait = 500 * time.Millisecond

// Store is what the worker needs from persistence. It is declared here rather
// than in board so that the API's interface stays as it is; *store.Store
// satisfies both.
type Store interface {
	// RunningJobs returns every running sheet with its owner.
	RunningJobs(ctx context.Context) ([]board.Job, error)
	// SaveProgress writes next's status, error, checklist and updated_at if the
	// sheet is still running and unchanged since it was read as seen. It
	// returns false, and no error, when it was not.
	SaveProgress(ctx context.Context, seen, next board.Job) (bool, error)
}

type Worker struct {
	store   Store
	dataDir string
	pause   time.Duration
	logger  *log.Logger
}

func New(store Store, dataDir string, pause time.Duration) *Worker {
	return &Worker{store: store, dataDir: dataDir, pause: pause, logger: log.Default()}
}

// Run moves sheets on until ctx is done, then returns nil. A database error is
// logged and the round is tried again later: nothing restarts a worker that
// exits, so it must not exit because Postgres went away for a while.
func (w *Worker) Run(ctx context.Context) error {
	for {
		wake, err := w.tick(ctx, time.Now())
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			w.logger.Printf("worker: %v", err)
		}
		timer := time.NewTimer(time.Until(wake))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// tick gives every running sheet that is due one step and returns when the
// next round should start. An error leaves the affected sheets as they were
// stored; they are due again on the next round.
func (w *Worker) tick(ctx context.Context, now time.Time) (time.Time, error) {
	// The database keeps microseconds. Saving exactly what it will hand back
	// keeps the wake-up computed here equal to the due time read next round.
	now = now.UTC().Truncate(time.Microsecond)
	wake := now.Add(maxWait)
	jobs, err := w.store.RunningJobs(ctx)
	if err != nil {
		return wake, fmt.Errorf("load running sheets: %w", err)
	}
	var errs []error
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if due := w.dueAt(job); now.Before(due) {
			if due.Before(wake) {
				wake = due
			}
			continue
		}
		step := firstPending(job.Checklist)
		next := advance(job, w.readable(job), now)
		saved, err := w.store.SaveProgress(ctx, job, next)
		if err != nil {
			errs = append(errs, fmt.Errorf("sheet %s run %s: save: %w", job.ID, job.RunID, err))
			continue
		}
		if !saved {
			// Someone else moved the sheet on since we read it; the next round
			// reads what it is now.
			continue
		}
		w.report(job, next, step)
		if next.Status == board.StatusRunning {
			if due := w.dueAt(next); due.Before(wake) {
				wake = due
			}
		}
	}
	return wake, errors.Join(errs...)
}

// dueAt is when a running sheet gets its next step. The first step comes at
// once. Each later one comes a pause after the previous step was saved, which
// is the sheet's updated_at, because the worker wrote that save.
func (w *Worker) dueAt(job board.Job) time.Time {
	for _, step := range job.Checklist {
		if step.State == board.StepDone {
			return job.UpdatedAt.Add(w.pause)
		}
	}
	return time.Time{}
}

// readable reports whether the whole brochure can be read. It is checked
// before every step: a brochure that disappears halfway fails the sheet too.
func (w *Worker) readable(job board.Job) bool {
	_, err := os.ReadFile(board.BrochurePath(w.dataDir, job.UserID, job.ID))
	return err == nil
}

func (w *Worker) report(job, next board.Job, step int) {
	if next.Status == board.StatusFailed {
		w.logger.Printf("sheet %s run %s: failed: %s", job.ID, job.RunID, next.Error)
		return
	}
	if step >= 0 && next.Checklist[step].State == board.StepDone {
		w.logger.Printf("sheet %s run %s: %q done", job.ID, job.RunID, next.Checklist[step].Label)
	}
	if next.Status == board.StatusSuccess {
		w.logger.Printf("sheet %s run %s: success", job.ID, job.RunID)
	}
}

// advance holds every rule about what a sheet becomes after one step. It does
// not touch job's checklist: the caller still needs job as it was read.
func advance(job board.Job, readable bool, now time.Time) board.Job {
	if job.Status != board.StatusRunning {
		return job
	}
	next := job
	next.Checklist = append([]board.Step(nil), job.Checklist...)
	next.UpdatedAt = now
	if !readable {
		next.Status = board.StatusFailed
		next.Error = board.BrochureMissing
		return next
	}
	if i := firstPending(next.Checklist); i >= 0 {
		next.Checklist[i].State = board.StepDone
	}
	// The last step and success are one save, so no stored sheet is ever
	// running with nothing left to do; one that somehow is just finishes.
	if firstPending(next.Checklist) < 0 {
		next.Status = board.StatusSuccess
	}
	return next
}

func firstPending(steps []board.Step) int {
	for i, step := range steps {
		if step.State == board.StepPending {
			return i
		}
	}
	return -1
}

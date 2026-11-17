package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"listingboard/internal/board"
)

// The tests spell out the README's texts instead of using the board
// constants, so that a wrong constant cannot hide a wrong result.
var labels = []string{"Reading the brochure", "Checking the address", "Publishing the sheet"}

const pause = 2 * time.Second

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// fakeStore keeps sheets in memory with the same guard as the SQL: a save
// lands only on a running sheet whose updated_at is still the one read.
type fakeStore struct {
	mu       sync.Mutex
	jobs     map[string]board.Job
	saves    []board.Job
	loadErr  error
	saveErr  error
	beforeSv func(ctx context.Context) // runs before a save, outside the lock
}

func newFakeStore(jobs ...board.Job) *fakeStore {
	f := &fakeStore{jobs: map[string]board.Job{}}
	for _, job := range jobs {
		f.jobs[job.ID] = clone(job)
	}
	return f
}

func clone(job board.Job) board.Job {
	job.Checklist = slices.Clone(job.Checklist)
	return job
}

func (f *fakeStore) RunningJobs(ctx context.Context) ([]board.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	var jobs []board.Job
	for _, job := range f.jobs {
		if job.Status == "running" {
			jobs = append(jobs, clone(job))
		}
	}
	slices.SortFunc(jobs, func(a, b board.Job) int {
		if c := a.UpdatedAt.Compare(b.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return jobs, nil
}

func (f *fakeStore) SaveProgress(ctx context.Context, seen, next board.Job) (bool, error) {
	f.mu.Lock()
	hook := f.beforeSv
	f.mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return false, f.saveErr
	}
	cur, ok := f.jobs[seen.ID]
	if !ok || cur.Status != "running" || !cur.UpdatedAt.Equal(seen.UpdatedAt) {
		return false, nil
	}
	cur.Status = next.Status
	cur.Error = next.Error
	cur.Checklist = slices.Clone(next.Checklist)
	cur.UpdatedAt = next.UpdatedAt
	f.jobs[cur.ID] = cur
	f.saves = append(f.saves, clone(cur))
	return true, nil
}

func (f *fakeStore) job(id string) board.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return clone(f.jobs[id])
}

func (f *fakeStore) saved() []board.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.saves)
}

// started is a sheet as POST /start leaves it: running, a run id, all pending.
func started(userID string, at time.Time) board.Job {
	steps := make([]board.Step, len(labels))
	for i, label := range labels {
		steps[i] = board.Step{Label: label, State: "pending"}
	}
	return board.Job{
		ID:        board.NewID(),
		UserID:    userID,
		Title:     "Market loft",
		Address:   "10 Market Street",
		Status:    "running",
		RunID:     board.NewID(),
		Filename:  "brochure.pdf",
		Checklist: steps,
		CreatedAt: at,
		UpdatedAt: at,
	}
}

func withDone(job board.Job, n int) board.Job {
	job = clone(job)
	for i := range n {
		job.Checklist[i].State = "done"
	}
	return job
}

func writeBrochure(t *testing.T, dataDir string, job board.Job) string {
	t.Helper()
	path := board.BrochurePath(dataDir, job.UserID, job.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("%PDF-1.4 brochure"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newWorker(st Store, dataDir string, p time.Duration, logs io.Writer) *Worker {
	w := New(st, dataDir, p)
	if logs == nil {
		logs = io.Discard
	}
	w.logger = log.New(logs, "", 0)
	return w
}

func states(job board.Job) []string {
	var out []string
	for _, step := range job.Checklist {
		out = append(out, step.State)
	}
	return out
}

func mustTick(t *testing.T, w *Worker, now time.Time) time.Time {
	t.Helper()
	wake, err := w.tick(context.Background(), now)
	if err != nil {
		t.Fatalf("tick at %s: %v", now, err)
	}
	return wake
}

func wantSheet(t *testing.T, got board.Job, status, errText string, steps ...string) {
	t.Helper()
	if got.Status != status || got.Error != errText || !slices.Equal(states(got), steps) {
		t.Fatalf("sheet is %q / %q / %v, want %q / %q / %v",
			got.Status, got.Error, states(got), status, errText, steps)
	}
}

func TestAdvance(t *testing.T) {
	base := started("ada", t0)
	now := t0.Add(time.Minute)
	draft := clone(base)
	draft.Status, draft.RunID = "draft", ""
	succeeded := withDone(base, 3)
	succeeded.Status = "success"
	failed := withDone(base, 1)
	failed.Status, failed.Error = "failed", "Brochure is missing"

	cases := []struct {
		name     string
		job      board.Job
		readable bool
		status   string
		errText  string
		steps    []string
		same     bool // returned unchanged, updated_at included
	}{
		{"a draft is left alone", draft, true, "draft", "", []string{"pending", "pending", "pending"}, true},
		{"a success is left alone", succeeded, true, "success", "", []string{"done", "done", "done"}, true},
		{"a failed sheet is left alone", failed, false, "failed", "Brochure is missing", []string{"done", "pending", "pending"}, true},
		{"unreadable before the first step", base, false, "failed", "Brochure is missing", []string{"pending", "pending", "pending"}, false},
		{"unreadable before the third step", withDone(base, 2), false, "failed", "Brochure is missing", []string{"done", "done", "pending"}, false},
		{"first step", base, true, "running", "", []string{"done", "pending", "pending"}, false},
		{"second step", withDone(base, 1), true, "running", "", []string{"done", "done", "pending"}, false},
		{"third step is success in the same value", withDone(base, 2), true, "success", "", []string{"done", "done", "done"}, false},
		{"running with nothing pending", withDone(base, 3), true, "success", "", []string{"done", "done", "done"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := clone(c.job)
			got := advance(c.job, c.readable, now)
			wantSheet(t, got, c.status, c.errText, c.steps...)
			if !slices.Equal(c.job.Checklist, before.Checklist) {
				t.Fatalf("the argument's checklist changed: %v, was %v", c.job.Checklist, before.Checklist)
			}
			if c.same && !got.UpdatedAt.Equal(c.job.UpdatedAt) {
				t.Fatalf("updated_at moved on a sheet that was left alone")
			}
			if !c.same && !got.UpdatedAt.Equal(now) {
				t.Fatalf("updated_at is %s, want %s", got.UpdatedAt, now)
			}
			if got.RunID != c.job.RunID || got.Filename != c.job.Filename || got.ID != c.job.ID ||
				got.UserID != c.job.UserID || got.Title != c.job.Title || got.Address != c.job.Address {
				t.Fatalf("advance changed a field that is not the worker's: %+v", got)
			}
		})
	}
}

func TestAdvanceDoesNotShareTheChecklist(t *testing.T) {
	job := started("ada", t0)
	got := advance(job, true, t0)
	got.Checklist[1].State = "done"
	if job.Checklist[1].State != "pending" {
		t.Fatal("the result shares its checklist with the argument")
	}
}

func TestAFreshSheetGetsItsFirstStepAtOnce(t *testing.T) {
	dir := t.TempDir()
	job := started("ada", t0)
	writeBrochure(t, dir, job)
	st := newFakeStore(job)
	var logs bytes.Buffer
	w := newWorker(st, dir, pause, &logs)

	// The API's updated_at may even be a little ahead of the worker's clock.
	now := t0.Add(-time.Second)
	wake := mustTick(t, w, now)
	wantSheet(t, st.job(job.ID), "running", "", "done", "pending", "pending")
	if want := now.Add(500 * time.Millisecond); !wake.Equal(want) {
		t.Fatalf("wake at %s, want %s (never later than 500 ms)", wake, want)
	}
	want := `sheet ` + job.ID + ` run ` + job.RunID + `: "Reading the brochure" done` + "\n"
	if logs.String() != want {
		t.Fatalf("log:\n%s\nwant:\n%s", logs.String(), want)
	}
}

func TestTheNextStepWaitsForThePause(t *testing.T) {
	dir := t.TempDir()
	job := started("ada", t0)
	writeBrochure(t, dir, job)
	st := newFakeStore(job)
	// A long pause, so the wake-up is the due time and not the 500 ms cap.
	const long = 10 * time.Second
	w := newWorker(st, dir, long, nil)

	first := t0.Add(time.Second)
	mustTick(t, w, first)
	due := first.Add(long)

	for _, now := range []time.Time{first.Add(time.Second), due.Add(-time.Microsecond)} {
		wake := mustTick(t, w, now)
		if n := len(st.saved()); n != 1 {
			t.Fatalf("tick at %s before the pause is over: %d saves, want 1", now, n)
		}
		want := now.Add(500 * time.Millisecond)
		if due.Before(want) {
			want = due
		}
		if !wake.Equal(want) {
			t.Fatalf("tick at %s: wake at %s, want %s", now, wake, want)
		}
	}
	mustTick(t, w, due)
	wantSheet(t, st.job(job.ID), "running", "", "done", "done", "pending")
}

func TestAWholeRunIsThreeSaves(t *testing.T) {
	dir := t.TempDir()
	job := started("ada", t0)
	writeBrochure(t, dir, job)
	st := newFakeStore(job)
	var logs bytes.Buffer
	w := newWorker(st, dir, pause, &logs)

	now := t0
	for range 10 {
		wake := mustTick(t, w, now)
		now = wake
	}
	saves := st.saved()
	if len(saves) != 3 {
		t.Fatalf("%d saves, want 3", len(saves))
	}
	wantSheet(t, saves[0], "running", "", "done", "pending", "pending")
	wantSheet(t, saves[1], "running", "", "done", "done", "pending")
	wantSheet(t, saves[2], "success", "", "done", "done", "done")
	for _, s := range saves {
		if s.RunID != job.RunID {
			t.Fatalf("run id changed to %q", s.RunID)
		}
	}
	if gap := saves[1].UpdatedAt.Sub(saves[0].UpdatedAt); gap != pause {
		t.Fatalf("second step %s after the first, want %s", gap, pause)
	}
	prefix := "sheet " + job.ID + " run " + job.RunID + ": "
	want := prefix + `"Reading the brochure" done` + "\n" +
		prefix + `"Checking the address" done` + "\n" +
		prefix + `"Publishing the sheet" done` + "\n" +
		prefix + "success\n"
	if logs.String() != want {
		t.Fatalf("log:\n%s\nwant:\n%s", logs.String(), want)
	}
}

func TestARestartedWorkerContinuesFromTheFirstPendingStep(t *testing.T) {
	for _, before := range []int{1, 2} {
		dir := t.TempDir()
		job := started("ada", t0)
		writeBrochure(t, dir, job)
		st := newFakeStore(job)

		now := t0
		first := newWorker(st, dir, pause, nil)
		for range before {
			mustTick(t, first, now)
			now = now.Add(pause)
		}
		if n := len(st.saved()); n != before {
			t.Fatalf("%d saves before the restart, want %d", n, before)
		}

		// A new process: nothing carried over but the store and the files.
		var logs bytes.Buffer
		second := newWorker(st, dir, pause, &logs)
		mustTick(t, second, now.Add(time.Hour))
		saves := st.saved()
		if len(saves) != before+1 {
			t.Fatalf("after %d steps: %d saves after the restart, want %d", before, len(saves), before+1)
		}
		last := saves[len(saves)-1]
		want := []string{"done", "done", "done"}
		status := "success"
		if before == 1 {
			want = []string{"done", "done", "pending"}
			status = "running"
		}
		wantSheet(t, last, status, "", want...)
		if last.RunID != job.RunID {
			t.Fatalf("run id is %q, want %q", last.RunID, job.RunID)
		}
		if strings.Contains(logs.String(), "Reading the brochure") ||
			(before == 2 && strings.Contains(logs.String(), "Checking the address")) {
			t.Fatalf("a done step was done again:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), `"`+labels[before]+`" done`) {
			t.Fatalf("the first pending step %q was not done:\n%s", labels[before], logs.String())
		}
	}
}

func TestABrochureThatCannotBeReadFailsTheSheet(t *testing.T) {
	cases := []struct {
		name  string
		after int // steps done before the brochure goes
		spoil func(t *testing.T, path string)
	}{
		{"missing at the first step", 0, func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{"removed after the first step", 1, func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{"a directory where the file should be", 0, func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			job := started("ada", t0)
			path := writeBrochure(t, dir, job)
			st := newFakeStore(job)
			var logs bytes.Buffer
			w := newWorker(st, dir, pause, &logs)
			now := t0
			for range c.after {
				mustTick(t, w, now)
				now = now.Add(pause)
			}
			c.spoil(t, path)
			mustTick(t, w, now)

			steps := []string{"pending", "pending", "pending"}
			for i := range c.after {
				steps[i] = "done"
			}
			got := st.job(job.ID)
			wantSheet(t, got, "failed", "Brochure is missing", steps...)
			if got.RunID != job.RunID {
				t.Fatalf("run id is %q, want %q", got.RunID, job.RunID)
			}
			want := "sheet " + job.ID + " run " + job.RunID + ": failed: Brochure is missing\n"
			if !strings.HasSuffix(logs.String(), want) {
				t.Fatalf("log:\n%s\nwant it to end with:\n%s", logs.String(), want)
			}
			// A failed sheet is no longer picked up.
			mustTick(t, w, now.Add(time.Hour))
			if n := len(st.saved()); n != c.after+1 {
				t.Fatalf("%d saves, want %d", n, c.after+1)
			}
		})
	}
}

func TestALoadErrorChangesNothing(t *testing.T) {
	dir := t.TempDir()
	job := started("ada", t0)
	writeBrochure(t, dir, job)
	st := newFakeStore(job)
	boom := errors.New("connection refused")
	st.loadErr = boom
	w := newWorker(st, dir, pause, nil)

	wake, err := w.tick(context.Background(), t0)
	if !errors.Is(err, boom) {
		t.Fatalf("tick: %v, want %v", err, boom)
	}
	if !wake.After(t0) || wake.After(t0.Add(500*time.Millisecond)) {
		t.Fatalf("wake at %s, want within 500 ms after %s", wake, t0)
	}
	wantSheet(t, st.job(job.ID), "running", "", "pending", "pending", "pending")
	if n := len(st.saved()); n != 0 {
		t.Fatalf("%d saves, want 0", n)
	}
}

func TestASaveErrorLeavesTheSheetForTheNextTick(t *testing.T) {
	dir := t.TempDir()
	job := started("ada", t0)
	writeBrochure(t, dir, job)
	st := newFakeStore(withDone(job, 1))
	boom := errors.New("connection reset")
	st.saveErr = boom
	w := newWorker(st, dir, pause, nil)

	now := t0.Add(pause)
	if _, err := w.tick(context.Background(), now); !errors.Is(err, boom) {
		t.Fatalf("tick: %v, want %v", err, boom)
	}
	got := st.job(job.ID)
	wantSheet(t, got, "running", "", "done", "pending", "pending")
	if !got.UpdatedAt.Equal(t0) {
		t.Fatalf("updated_at is %s, want %s", got.UpdatedAt, t0)
	}

	st.mu.Lock()
	st.saveErr = nil
	st.mu.Unlock()
	mustTick(t, w, now.Add(time.Millisecond))
	saves := st.saved()
	if len(saves) != 1 {
		t.Fatalf("%d saves, want 1", len(saves))
	}
	wantSheet(t, saves[0], "running", "", "done", "done", "pending")
}

func TestACancelledContextNeverFailsASheet(t *testing.T) {
	t.Run("before the tick", func(t *testing.T) {
		dir := t.TempDir()
		job := started("ada", t0)
		// No brochure at all: even then, a tick that cannot run decides nothing.
		st := newFakeStore(job)
		w := newWorker(st, dir, pause, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := w.tick(ctx, t0); err == nil {
			t.Fatal("tick with a cancelled context returned no error")
		}
		wantSheet(t, st.job(job.ID), "running", "", "pending", "pending", "pending")
	})
	t.Run("during the tick", func(t *testing.T) {
		dir := t.TempDir()
		job := started("ada", t0)
		writeBrochure(t, dir, job)
		other := started("grace", t0.Add(time.Microsecond))
		writeBrochure(t, dir, other)
		st := newFakeStore(job, other)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		st.beforeSv = func(context.Context) { cancel() }
		w := newWorker(st, dir, pause, nil)
		if _, err := w.tick(ctx, t0); err == nil {
			t.Fatal("tick cancelled halfway returned no error")
		}
		for _, id := range []string{job.ID, other.ID} {
			wantSheet(t, st.job(id), "running", "", "pending", "pending", "pending")
		}
	})
}

func TestRunReturnsWhenItsContextIsCancelled(t *testing.T) {
	st := newFakeStore()
	w := newWorker(st, t.TempDir(), pause, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func TestASheetChangedByOthersIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	job := started("ada", t0)
	writeBrochure(t, dir, job)
	st := newFakeStore(job)
	// Another worker instance takes the first step between our read and our save.
	theirs := withDone(job, 1)
	theirs.UpdatedAt = t0.Add(time.Millisecond)
	st.beforeSv = func(context.Context) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.jobs[job.ID] = clone(theirs)
		st.beforeSv = nil
	}
	var logs bytes.Buffer
	w := newWorker(st, dir, pause, &logs)
	mustTick(t, w, t0.Add(time.Millisecond))
	got := st.job(job.ID)
	wantSheet(t, got, "running", "", "done", "pending", "pending")
	if !got.UpdatedAt.Equal(theirs.UpdatedAt) {
		t.Fatalf("updated_at is %s, want theirs %s", got.UpdatedAt, theirs.UpdatedAt)
	}
	if n := len(st.saved()); n != 0 {
		t.Fatalf("%d saves landed, want 0", n)
	}
	if logs.Len() != 0 {
		t.Fatalf("logged a change that was not saved:\n%s", logs.String())
	}
}

func TestSheetsOfTwoAccountsMoveInTheSameTick(t *testing.T) {
	dir := t.TempDir()
	ada := started("ada", t0)
	grace := started("grace", t0.Add(time.Second))
	writeBrochure(t, dir, ada)
	writeBrochure(t, dir, grace)
	st := newFakeStore(ada, grace)
	w := newWorker(st, dir, pause, nil)
	mustTick(t, w, t0.Add(2*time.Second))
	for _, id := range []string{ada.ID, grace.ID} {
		wantSheet(t, st.job(id), "running", "", "done", "pending", "pending")
	}
}

func TestRunTakesASheetToSuccess(t *testing.T) {
	dir := t.TempDir()
	job := started("ada", time.Now().UTC())
	writeBrochure(t, dir, job)
	st := newFakeStore(job)
	var logs syncBuffer
	w := newWorker(st, dir, 5*time.Millisecond, &logs)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	stop := func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for st.job(job.ID).Status == "running" {
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("still running after 5s: %v", states(st.job(job.ID)))
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The store shows success before Run has logged that save; stopping Run
	// first makes its log complete.
	stop()
	wantSheet(t, st.job(job.ID), "success", "", "done", "done", "done")
	if n := len(st.saved()); n != 3 {
		t.Fatalf("%d saves, want 3", n)
	}
	for _, label := range labels {
		if n := strings.Count(logs.String(), `"`+label+`" done`); n != 1 {
			t.Fatalf("%q logged %d times, want once:\n%s", label, n, logs.String())
		}
	}
}

// syncBuffer is a bytes.Buffer that Run's goroutine and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

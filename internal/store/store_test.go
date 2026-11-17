package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"listingboard/internal/board"
)

// freshDatabase creates an empty database on the server named by
// TEST_DATABASE_URL, drops it when the test ends, and returns its DSN. Tests
// are skipped without that variable, because go test ./... needs no Postgres.
func freshDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := "lbtest_" + board.NewID()
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func openStore(t *testing.T, dsn string) *Store {
	t.Helper()
	st, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newUser(t *testing.T, st *Store, email string) board.User {
	t.Helper()
	user, err := st.CreateUser(context.Background(), email, "hash")
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func newDraft(t *testing.T, st *Store, userID string) board.Job {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	job := board.Job{
		ID:        board.NewID(),
		UserID:    userID,
		Title:     "Market loft",
		Address:   "10 Market Street",
		Status:    board.StatusDraft,
		Filename:  "brochure.pdf",
		Checklist: board.NewChecklist(),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := st.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

func running(job board.Job) board.Job {
	job.Status = board.StatusRunning
	job.RunID = board.NewID()
	return job
}

func TestOpenAtTheSameMomentOnAFreshDatabase(t *testing.T) {
	t.Parallel()
	dsn := freshDatabase(t)
	const opens = 8
	stores := make([]*Store, opens)
	errs := make([]error, opens)
	var ready, wg sync.WaitGroup
	gate := make(chan struct{})
	for i := range opens {
		ready.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			<-gate
			stores[i], errs[i] = Open(dsn)
		}()
	}
	ready.Wait()
	close(gate)
	wg.Wait()
	for _, st := range stores {
		if st != nil {
			t.Cleanup(func() { _ = st.Close() })
		}
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("open %d: %v", i, err)
		}
	}
}

func TestOpenTwiceMigratesOnce(t *testing.T) {
	t.Parallel()
	dsn := freshDatabase(t)
	first := openStore(t, dsn)
	user := newUser(t, first, "ada@example.com")
	openStore(t, dsn)
	if _, err := first.UserByEmail(context.Background(), user.Email); err != nil {
		t.Fatalf("the second open lost data: %v", err)
	}
}

func TestAStaleUpdateDoesNotOverwriteARunningSheet(t *testing.T) {
	t.Parallel()
	st := openStore(t, freshDatabase(t))
	ctx := context.Background()
	user := newUser(t, st, "ada@example.com")
	draft := newDraft(t, st, user.ID)

	draft.Filename = "other.pdf"
	if err := st.UpdateJob(ctx, draft); err != nil {
		t.Fatalf("update a draft: %v", err)
	}
	started := running(draft)
	if err := st.UpdateJob(ctx, started); err != nil {
		t.Fatalf("start the draft: %v", err)
	}

	stale := draft
	stale.Filename = "stale.pdf"
	if err := st.UpdateJob(ctx, stale); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("stale update: %v, want ErrNotFound", err)
	}
	got, err := st.Job(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != board.StatusRunning || got.RunID != started.RunID || got.Filename != "other.pdf" ||
		!slices.Equal(got.Checklist, started.Checklist) {
		t.Fatalf("row changed: %+v, want %+v", got, started)
	}
}

func TestPostgresAdmitsOneRunningSheetPerAccount(t *testing.T) {
	t.Parallel()
	st := openStore(t, freshDatabase(t))
	ctx := context.Background()
	ada := newUser(t, st, "ada@example.com")
	grace := newUser(t, st, "grace@example.com")
	first := newDraft(t, st, ada.ID)
	second := newDraft(t, st, ada.ID)
	theirs := newDraft(t, st, grace.ID)

	if err := st.UpdateJob(ctx, running(first)); err != nil {
		t.Fatalf("start the first: %v", err)
	}
	if err := st.UpdateJob(ctx, running(second)); !errors.Is(err, board.ErrDuplicate) {
		t.Fatalf("start the second: %v, want ErrDuplicate", err)
	}
	got, err := st.Job(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != board.StatusDraft || got.RunID != "" {
		t.Fatalf("second sheet is %s with runId %q, want a draft without one", got.Status, got.RunID)
	}
	if err := st.UpdateJob(ctx, running(theirs)); err != nil {
		t.Fatalf("another account's sheet: %v", err)
	}
}

// unreachable refuses connections at once; Postgres never listens on port 1.
const unreachable = "postgres://board:board@127.0.0.1:1/board?sslmode=disable"

func TestWaitGivesUpAfterItsPatience(t *testing.T) {
	t.Parallel()
	const patience = 1500 * time.Millisecond
	began := time.Now()
	st, err := Wait(context.Background(), unreachable, patience)
	took := time.Since(began)
	if err == nil {
		_ = st.Close()
		t.Fatal("Wait connected to nothing")
	}
	if took < patience || took > patience+2*time.Second {
		t.Fatalf("Wait gave up after %s, want about %s", took, patience)
	}
}

func TestWaitStopsWhenItsContextIsDone(t *testing.T) {
	t.Parallel()
	// A non-routable address leaves the connect hanging, so this also checks
	// that an attempt in flight is abandoned, not only the pause between them.
	for _, dsn := range []string{unreachable, "postgres://board:board@10.255.255.1:5432/board?sslmode=disable"} {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(300*time.Millisecond, cancel)
		began := time.Now()
		st, err := Wait(ctx, dsn, time.Minute)
		took := time.Since(began)
		cancel()
		if err == nil {
			_ = st.Close()
			t.Fatalf("%s: Wait connected to nothing", dsn)
		}
		if took > 2*time.Second {
			t.Fatalf("%s: Wait returned %s after its context was cancelled", dsn, took-300*time.Millisecond)
		}
	}
}

// start moves a draft to running through the API's own write, as POST /start does.
func start(t *testing.T, st *Store, draft board.Job, at time.Time) board.Job {
	t.Helper()
	job := running(draft)
	job.UpdatedAt = at
	if err := st.UpdateJob(context.Background(), job); err != nil {
		t.Fatalf("start: %v", err)
	}
	return job
}

func TestRunningJobsListsRunningSheetsOldestFirst(t *testing.T) {
	t.Parallel()
	st := openStore(t, freshDatabase(t))
	ctx := context.Background()
	ada := newUser(t, st, "ada@example.com")
	grace := newUser(t, st, "grace@example.com")
	linus := newUser(t, st, "linus@example.com")
	base := time.Now().UTC().Truncate(time.Microsecond)

	newer := start(t, st, newDraft(t, st, ada.ID), base.Add(time.Minute))
	older := start(t, st, newDraft(t, st, grace.ID), base)
	newDraft(t, st, ada.ID)
	finished := start(t, st, newDraft(t, st, linus.ID), base.Add(-time.Minute))
	done := finished
	done.Status = board.StatusSuccess
	done.UpdatedAt = base.Add(-time.Second)
	if ok, err := st.SaveProgress(ctx, finished, done); err != nil || !ok {
		t.Fatalf("finish a sheet: %v, %v", ok, err)
	}

	jobs, err := st.RunningJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("%d running sheets, want 2: %+v", len(jobs), jobs)
	}
	for i, want := range []board.Job{older, newer} {
		got := jobs[i]
		if got.ID != want.ID || got.UserID != want.UserID || got.RunID != want.RunID ||
			got.Status != "running" || !got.UpdatedAt.Equal(want.UpdatedAt) {
			t.Fatalf("sheet %d is %+v, want %+v", i, got, want)
		}
	}
}

func TestSaveProgressWritesOnlyTheWorkersFields(t *testing.T) {
	t.Parallel()
	st := openStore(t, freshDatabase(t))
	ctx := context.Background()
	user := newUser(t, st, "ada@example.com")
	seen := start(t, st, newDraft(t, st, user.ID), time.Now().UTC().Truncate(time.Microsecond))

	next := seen
	next.Checklist = slices.Clone(seen.Checklist)
	next.Checklist[0].State = board.StepDone
	next.Status = board.StatusFailed
	next.Error = "Brochure is missing"
	next.UpdatedAt = seen.UpdatedAt.Add(2 * time.Second)
	next.RunID = "another run"
	next.Filename = "other.pdf"
	next.Title = "Other title"
	next.Address = "Other address"
	ok, err := st.SaveProgress(ctx, seen, next)
	if err != nil || !ok {
		t.Fatalf("save: %v, %v", ok, err)
	}
	got, err := st.Job(ctx, seen.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.Error != "Brochure is missing" || !slices.Equal(got.Checklist, next.Checklist) ||
		!got.UpdatedAt.Equal(next.UpdatedAt) {
		t.Fatalf("worker fields: %+v, want %+v", got, next)
	}
	if got.RunID != seen.RunID || got.Filename != seen.Filename || got.Title != seen.Title || got.Address != seen.Address {
		t.Fatalf("API fields changed: %+v, want %+v", got, seen)
	}
}

func TestSaveProgressSkipsASheetThatChanged(t *testing.T) {
	t.Parallel()
	st := openStore(t, freshDatabase(t))
	ctx := context.Background()
	user := newUser(t, st, "ada@example.com")
	other := newUser(t, st, "grace@example.com")
	at := time.Now().UTC().Truncate(time.Microsecond)

	draft := newDraft(t, st, user.ID)
	runningSheet := start(t, st, newDraft(t, st, other.ID), at)

	cases := []struct {
		name string
		seen board.Job
		want board.Job
	}{
		// A draft belongs to the API, even when updated_at matches.
		{"not running", running(draft), draft},
		{"stale updated_at", func() board.Job {
			stale := runningSheet
			stale.UpdatedAt = at.Add(-time.Second)
			return stale
		}(), runningSheet},
	}
	for _, c := range cases {
		next := c.seen
		next.Checklist = slices.Clone(c.seen.Checklist)
		next.Checklist[0].State = board.StepDone
		next.UpdatedAt = at.Add(time.Hour)
		ok, err := st.SaveProgress(ctx, c.seen, next)
		if err != nil || ok {
			t.Fatalf("%s: save returned %v, %v; want false, nil", c.name, ok, err)
		}
		got, err := st.Job(ctx, c.want.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != c.want.Status || !slices.Equal(got.Checklist, c.want.Checklist) ||
			!got.UpdatedAt.Equal(c.want.UpdatedAt) {
			t.Fatalf("%s: row changed: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestASheetReadBackCanBeSavedAgain(t *testing.T) {
	t.Parallel()
	st := openStore(t, freshDatabase(t))
	ctx := context.Background()
	user := newUser(t, st, "ada@example.com")
	// Nanoseconds that Postgres cannot keep, as time.Now gives them.
	at := time.Now().UTC().Truncate(time.Microsecond).Add(789 * time.Nanosecond)
	start(t, st, newDraft(t, st, user.ID), at)

	for step := range 3 {
		jobs, err := st.RunningJobs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 1 {
			t.Fatalf("step %d: %d running sheets, want 1", step, len(jobs))
		}
		seen := jobs[0]
		next := seen
		next.Checklist = slices.Clone(seen.Checklist)
		next.Checklist[step].State = board.StepDone
		next.UpdatedAt = time.Now().Add(time.Duration(step+1) * 123 * time.Nanosecond)
		ok, err := st.SaveProgress(ctx, seen, next)
		if err != nil || !ok {
			t.Fatalf("step %d: save of a sheet read back returned %v, %v", step, ok, err)
		}
	}
	jobs, err := st.RunningJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range jobs[0].Checklist {
		if step.State != "done" {
			t.Fatalf("checklist %+v, want every step done", jobs[0].Checklist)
		}
	}
}

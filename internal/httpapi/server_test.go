package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"listingboard/internal/board"
)

// The README names this test, so it keeps its name. It used to describe a
// handler that finished the sheet on its own; it now describes the spec:
// start moves a draft to running, chooses its run id, and leaves every step
// to the worker.
func TestStartMarksTheJobSuccessful(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")

	res := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	got := decodeJob(t, res)
	if got.Status != "running" {
		t.Fatalf("status %s, want running", got.Status)
	}
	if got.RunID == "" {
		t.Fatal("runId is empty")
	}
	if got.Error != "" {
		t.Fatalf("error %q, want none", got.Error)
	}
	labels := []string{"Reading the brochure", "Checking the address", "Publishing the sheet"}
	if len(got.Checklist) != len(labels) {
		t.Fatalf("%d steps, want %d", len(got.Checklist), len(labels))
	}
	for i, step := range got.Checklist {
		if step.Label != labels[i] {
			t.Fatalf("step %d is %q, want %q", i, step.Label, labels[i])
		}
		if step.State != "pending" {
			t.Fatalf("step %q is %s, want pending", step.Label, step.State)
		}
	}

	// The handler stored what it answered: the sheet is running, not finished.
	saved := getSheet(t, srv, cookie, job.ID)
	if saved.Status != "running" || saved.RunID != got.RunID {
		t.Fatalf("stored sheet is %s with runId %q, want running with %q", saved.Status, saved.RunID, got.RunID)
	}
	for _, step := range saved.Checklist {
		if step.State != "pending" {
			t.Fatalf("stored step %q is %s, want pending", step.Label, step.State)
		}
	}
}

func TestStartingARunningSheetAgainKeepsItsRunID(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := readySheet(t, srv, cookie, "Market loft")
	first := startOK(t, srv, cookie, job.ID)

	res := startSheet(srv, cookie, job.ID)
	if res.Code != http.StatusOK {
		t.Fatalf("second start: status %d: %s", res.Code, res.Body.String())
	}
	again := decodeJob(t, res)
	if again.Status != "running" || again.RunID != first.RunID {
		t.Fatalf("second start answered %s with runId %q, want running with %q", again.Status, again.RunID, first.RunID)
	}
	saved := getSheet(t, srv, cookie, job.ID)
	if saved.Status != "running" || saved.RunID != first.RunID {
		t.Fatalf("stored sheet is %s with runId %q, want running with %q", saved.Status, saved.RunID, first.RunID)
	}
	for _, step := range append(again.Checklist, saved.Checklist...) {
		if step.State != "pending" {
			t.Fatalf("step %q is %s, want pending", step.Label, step.State)
		}
	}
}

func TestStartWithoutABrochureAsksForOne(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

	res := startSheet(srv, cookie, job.ID)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.Code, res.Body.String())
	}
	if got := errorText(t, res); got != "Attach a brochure first" {
		t.Fatalf("error %q, want %q", got, "Attach a brochure first")
	}
	saved := getSheet(t, srv, cookie, job.ID)
	if saved.Status != "draft" || saved.RunID != "" {
		t.Fatalf("stored sheet is %s with runId %q, want a draft without one", saved.Status, saved.RunID)
	}
}

func TestASecondSheetCannotStartWhileOneIsRunning(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	ada := signUp(t, srv, "ada@example.com")
	grace := signUp(t, srv, "grace@example.com")
	first := readySheet(t, srv, ada, "Market loft")
	second := readySheet(t, srv, ada, "Harbour flat")
	theirs := readySheet(t, srv, grace, "Garden house")
	running := startOK(t, srv, ada, first.ID)

	res := startSheet(srv, ada, second.ID)
	if res.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", res.Code, res.Body.String())
	}
	if got := errorText(t, res); got != "Another sheet is already running" {
		t.Fatalf("error %q, want %q", got, "Another sheet is already running")
	}
	if saved := getSheet(t, srv, ada, second.ID); saved.Status != "draft" || saved.RunID != "" {
		t.Fatalf("second sheet is %s with runId %q, want a draft without one", saved.Status, saved.RunID)
	}
	if saved := getSheet(t, srv, ada, first.ID); !sameRun(saved, running) {
		t.Fatalf("first sheet changed: %+v, was %+v", saved, running)
	}

	// The limit is per account: another account starts its own sheet.
	if other := startOK(t, srv, grace, theirs.ID); other.Status != "running" {
		t.Fatalf("other account's sheet is %s, want running", other.Status)
	}
}

func TestADraftWithoutABrochureAsksForOneEvenWhileAnotherRuns(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	first := readySheet(t, srv, cookie, "Market loft")
	bare := createSheet(t, srv, cookie, "Harbour flat", "2 Harbour Road")
	startOK(t, srv, cookie, first.ID)

	res := startSheet(srv, cookie, bare.ID)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.Code, res.Body.String())
	}
	if got := errorText(t, res); got != "Attach a brochure first" {
		t.Fatalf("error %q, want %q", got, "Attach a brochure first")
	}
}

func TestStartLeavesFinishedSheetsAsTheyAre(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	cases := []struct {
		status, error string
		states        []string
	}{
		{"failed", "Brochure is missing", []string{"done", "pending", "pending"}},
		{"success", "", []string{"done", "done", "done"}},
	}
	for _, c := range cases {
		job := readySheet(t, srv, cookie, "Sheet "+c.status)
		finished := setSheet(t, srv, job.ID, func(job *board.Job) {
			job.Status = c.status
			job.Error = c.error
			job.RunID = board.NewID()
			for i := range job.Checklist {
				job.Checklist[i].State = c.states[i]
			}
		})

		res := startSheet(srv, cookie, job.ID)
		if res.Code != http.StatusOK {
			t.Fatalf("%s sheet: status %d: %s", c.status, res.Code, res.Body.String())
		}
		if got := decodeJob(t, res); !sameRun(got, finished) {
			t.Errorf("%s sheet: answered %+v, want %+v", c.status, got, finished)
		}
		if saved := getSheet(t, srv, cookie, job.ID); !sameRun(saved, finished) {
			t.Errorf("%s sheet: stored %+v, want %+v", c.status, saved, finished)
		}
	}
}

func TestTheNextSheetStartsOnceTheRunningOneIsFinished(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	first := readySheet(t, srv, cookie, "Market loft")
	second := readySheet(t, srv, cookie, "Harbour flat")
	startOK(t, srv, cookie, first.ID)
	if res := startSheet(srv, cookie, second.ID); res.Code != http.StatusConflict {
		t.Fatalf("while the first runs: status %d, want 409", res.Code)
	}

	setSheet(t, srv, first.ID, func(job *board.Job) { job.Status = "success" })
	if got := startOK(t, srv, cookie, second.ID); got.Status != "running" || got.RunID == "" {
		t.Fatalf("second sheet is %s with runId %q, want running with one", got.Status, got.RunID)
	}
}

func TestSimultaneousStartsOfOneDraftShareOneRunID(t *testing.T) {
	t.Parallel()
	srv, _ := newSlowServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := readySheet(t, srv, cookie, "Market loft")

	const starts = 16
	results := make([]*httptest.ResponseRecorder, starts)
	var ready, wg sync.WaitGroup
	gate := make(chan struct{})
	for i := range results {
		ready.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			<-gate
			results[i] = startSheet(srv, cookie, job.ID)
		}()
	}
	ready.Wait()
	close(gate)
	wg.Wait()

	runIDs := map[string]bool{}
	for i, res := range results {
		if res.Code != http.StatusOK {
			t.Fatalf("start %d: status %d: %s", i, res.Code, res.Body.String())
		}
		got := decodeJob(t, res)
		if got.Status != "running" {
			t.Fatalf("start %d answered %s, want running", i, got.Status)
		}
		runIDs[got.RunID] = true
	}
	if len(runIDs) != 1 {
		t.Fatalf("%d run ids for one sheet: %v", len(runIDs), runIDs)
	}
	saved := getSheet(t, srv, cookie, job.ID)
	if !runIDs[saved.RunID] {
		t.Fatalf("stored runId %q is not the one answered: %v", saved.RunID, runIDs)
	}
}

func TestSimultaneousStartsOfTwoDraftsAdmitOne(t *testing.T) {
	t.Parallel()
	srv, _ := newSlowServer(t)
	cookie := signUp(t, srv, "ada@example.com")

	for round := range 20 {
		ids := []string{
			readySheet(t, srv, cookie, fmt.Sprintf("Loft %d", round)).ID,
			readySheet(t, srv, cookie, fmt.Sprintf("Flat %d", round)).ID,
		}
		results := make([]*httptest.ResponseRecorder, len(ids))
		var ready, wg sync.WaitGroup
		gate := make(chan struct{})
		for i, id := range ids {
			ready.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				ready.Done()
				<-gate
				results[i] = startSheet(srv, cookie, id)
			}()
		}
		ready.Wait()
		close(gate)
		wg.Wait()

		codes := []int{results[0].Code, results[1].Code}
		slices.Sort(codes)
		if !slices.Equal(codes, []int{http.StatusOK, http.StatusConflict}) {
			t.Fatalf("round %d: statuses %v, want one 200 and one 409", round, codes)
		}
		var running []string
		for _, job := range listSheets(t, srv, cookie) {
			if job.Status == "running" {
				running = append(running, job.ID)
			}
		}
		if len(running) != 1 {
			t.Fatalf("round %d: %d sheets running, want 1", round, len(running))
		}
		setSheet(t, srv, running[0], func(job *board.Job) { job.Status = "success" })
	}
}

func TestUploadKeepsTheClientFilename(t *testing.T) {
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	userID := accountID(t, srv, cookie)
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	brochure := []byte("%PDF-1.4\n")
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", brochure, "application/pdf")

	// The name the client gave is kept in the sheet's filename field...
	if got := getSheet(t, srv, cookie, job.ID).Filename; got != "brochure.pdf" {
		t.Fatalf("filename %q, want brochure.pdf", got)
	}
	// ...and only there: the bytes are stored at {DATA_DIR}/{userID}/{jobID}/body.
	path := filepath.Join(dir, userID, job.ID, "body")
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("brochure was not stored at %s: %v", path, err)
	}
	if !bytes.Equal(stored, brochure) {
		t.Fatalf("stored %q, want %q", stored, brochure)
	}
	want := []string{filepath.Join(userID, job.ID, "body")}
	if files := filesUnder(t, dir); !slices.Equal(files, want) {
		t.Fatalf("files under the data dir are %v, want %v", files, want)
	}
}

func TestFilenamesThatLeaveTheDirectoryStayData(t *testing.T) {
	t.Parallel()
	names := []string{
		"../../escaped.txt",
		"../../../../escaped.txt",
		"/etc/escaped.txt",
		`..\..\escaped.txt`,
		"a/b/c.txt",
	}
	for _, name := range names {
		parent := t.TempDir()
		dir := filepath.Join(parent, "data")
		srv := New(newMem(), dir, "")
		cookie := signUp(t, srv, "ada@example.com")
		userID := accountID(t, srv, cookie)
		job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

		res := postFile(t, srv, cookie, job.ID, name, []byte("plain text\n"), "text/plain")
		if res.Code != http.StatusOK {
			t.Errorf("%q: status %d: %s", name, res.Code, res.Body.String())
			continue
		}
		if got := decodeJob(t, res).Filename; got != name {
			t.Errorf("%q: answered filename %q", name, got)
		}
		if got := getSheet(t, srv, cookie, job.ID).Filename; got != name {
			t.Errorf("%q: stored filename %q", name, got)
		}
		want := []string{filepath.Join("data", userID, job.ID, "body")}
		if files := filesUnder(t, parent); !slices.Equal(files, want) {
			t.Errorf("%q: files are %v, want %v", name, files, want)
		}
	}
}

func TestBrochureSizeLimitIsOneMiB(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	pdf := func(n int) []byte { return append([]byte("%PDF-"), bytes.Repeat([]byte{'x'}, n-5)...) }
	text := func(n int) []byte { return bytes.Repeat([]byte{'x'}, n) }
	cases := []struct {
		name string
		data []byte
		want int
	}{
		{"text of exactly 1 MiB", text(mib), http.StatusOK},
		{"pdf of exactly 1 MiB", pdf(mib), http.StatusOK},
		{"text one byte over", text(mib + 1), http.StatusBadRequest},
		{"pdf one byte over", pdf(mib + 1), http.StatusBadRequest},
		{"text of 3 MiB", text(3 * mib), http.StatusBadRequest},
	}
	for _, c := range cases {
		srv, dir := newTestServer(t)
		cookie := signUp(t, srv, "ada@example.com")
		job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

		res := postFile(t, srv, cookie, job.ID, "brochure.txt", c.data, "text/plain")
		if res.Code != c.want {
			t.Errorf("%s: status %d, want %d: %s", c.name, res.Code, c.want, res.Body.String())
			continue
		}
		if c.want == http.StatusOK {
			continue
		}
		if got := errorText(t, res); got != "Brochure is too large" {
			t.Errorf("%s: error %q, want %q", c.name, got, "Brochure is too large")
		}
		assertNothingAttached(t, srv, cookie, job.ID, dir)
	}
}

func TestBrochureTypeComesFromTheBytes(t *testing.T) {
	t.Parallel()
	filler := bytes.Repeat([]byte("a"), 600)
	cases := []struct {
		name        string
		filename    string
		contentType string
		data        []byte
		ok          bool
	}{
		{"pdf bytes named .txt, sent as text", "notes.txt", "text/plain", []byte("%PDF-1.4\n"), true},
		{"text named .pdf, sent as pdf", "scan.pdf", "application/pdf", []byte("just words\n"), true},
		{"pdf header followed by binary", "scan.pdf", "application/pdf", []byte("%PDF-1.4\n\x00\xff\xfe\x80"), true},
		{"pdf header alone", "scan.pdf", "application/pdf", []byte("%PDF-"), true},
		{"text that looks like html", "page.html", "text/html", []byte("<!DOCTYPE html><html><body>hi</body></html>"), true},
		{"text with a BOM", "notes.txt", "text/plain", []byte("\xef\xbb\xbfhello"), true},
		{"non-ASCII text", "notes.txt", "text/plain", []byte("Квартира на Ринковій, 10 \u00e9\u4e2d"), true},
		{"text with a control byte", "notes.txt", "text/plain", []byte("a\x01b"), true},
		{"bytes that are neither", "scan.pdf", "application/pdf", []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xfe}, false},
		{"text with a NUL", "notes.txt", "text/plain", []byte("a\x00b"), false},
		{"invalid UTF-8", "notes.txt", "text/plain", []byte("caf\xe9"), false},
		{"NUL at byte 600", "notes.txt", "text/plain", append(slices.Clone(filler), 0, 'b'), false},
		{"invalid byte at byte 600", "notes.txt", "text/plain", append(slices.Clone(filler), 0xff, 'b'), false},
		{"UTF-16 with a BOM", "notes.txt", "text/plain", []byte("\xff\xfeh\x00i\x00"), false},
		{"empty file", "notes.txt", "text/plain", []byte{}, false},
	}
	for _, c := range cases {
		srv, dir := newTestServer(t)
		cookie := signUp(t, srv, "ada@example.com")
		job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

		res := postFile(t, srv, cookie, job.ID, c.filename, c.data, c.contentType)
		if c.ok {
			if res.Code != http.StatusOK {
				t.Errorf("%s: status %d: %s", c.name, res.Code, res.Body.String())
			}
			continue
		}
		if res.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", c.name, res.Code, res.Body.String())
			continue
		}
		if got := errorText(t, res); got != "Brochure must be a PDF or a text file" {
			t.Errorf("%s: error %q, want %q", c.name, got, "Brochure must be a PDF or a text file")
		}
		assertNothingAttached(t, srv, cookie, job.ID, dir)
	}
}

func TestASecondUploadIsRefusedAndLeavesTheFirst(t *testing.T) {
	t.Parallel()
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	first := []byte("first brochure\n")
	uploadFile(t, srv, cookie, job.ID, "first.txt", first, "text/plain")
	before := filesUnder(t, dir)

	res := postFile(t, srv, cookie, job.ID, "second.pdf", []byte("%PDF-1.7\n"), "application/pdf")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.Code, res.Body.String())
	}
	if got := errorText(t, res); got != "This sheet already has a brochure" {
		t.Fatalf("error %q, want %q", got, "This sheet already has a brochure")
	}
	if got := getSheet(t, srv, cookie, job.ID).Filename; got != "first.txt" {
		t.Fatalf("filename %q, want first.txt", got)
	}
	files := filesUnder(t, dir)
	if len(files) != 1 || !slices.Equal(files, before) {
		t.Fatalf("files under the data dir are %v, were %v", files, before)
	}
	stored, err := os.ReadFile(filepath.Join(dir, files[0]))
	if err != nil || !bytes.Equal(stored, first) {
		t.Fatalf("stored %q (%v), want %q", stored, err, first)
	}
}

func TestSheetsThatHaveStartedTakeNoBrochure(t *testing.T) {
	t.Parallel()
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")

	// Started through the API: it has a brochure and is running.
	started := readySheet(t, srv, cookie, "Market loft")
	startOK(t, srv, cookie, started.ID)
	before := filesUnder(t, dir)
	res := postFile(t, srv, cookie, started.ID, "other.txt", []byte("other"), "text/plain")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("running sheet: status %d, want 400: %s", res.Code, res.Body.String())
	}
	if got := getSheet(t, srv, cookie, started.ID); got.Status != "running" || got.Filename != "brochure.pdf" {
		t.Fatalf("running sheet is %s with filename %q", got.Status, got.Filename)
	}

	// Sheets without a brochure that are no longer drafts.
	for _, status := range []string{"running", "success", "failed"} {
		job := createSheet(t, srv, cookie, "Sheet "+status, "10 Market Street")
		setSheet(t, srv, job.ID, func(job *board.Job) { job.Status = status })

		res := postFile(t, srv, cookie, job.ID, "late.txt", []byte("late"), "text/plain")
		if res.Code != http.StatusBadRequest {
			t.Errorf("%s sheet: status %d, want 400: %s", status, res.Code, res.Body.String())
			continue
		}
		if got := errorText(t, res); got != "This sheet has already been started" {
			t.Errorf("%s sheet: error %q, want %q", status, got, "This sheet has already been started")
		}
		if got := getSheet(t, srv, cookie, job.ID); got.Filename != "" || got.Status != status {
			t.Errorf("%s sheet changed: status %s, filename %q", status, got.Status, got.Filename)
		}
	}
	if after := filesUnder(t, dir); !slices.Equal(after, before) {
		t.Errorf("files under the data dir are %v, were %v", after, before)
	}
}

func TestUploadNeedsAFileWithAUsableName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		filename string
		ok       bool
	}{
		{"only spaces", "   ", false},
		{"256 bytes", strings.Repeat("a", 252) + ".txt", false},
		{"a tab", "bro\tchure.txt", false},
		{"a C1 control character", "bro\u0085chure.txt", false},
		{"255 bytes", strings.Repeat("a", 251) + ".txt", true},
		{"non-ASCII", "план будинку.pdf", true},
	}
	for _, c := range cases {
		srv, dir := newTestServer(t)
		cookie := signUp(t, srv, "ada@example.com")
		job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

		res := postFile(t, srv, cookie, job.ID, c.filename, []byte("%PDF-1.4\n"), "application/pdf")
		if c.ok {
			if res.Code != http.StatusOK {
				t.Errorf("%s: status %d: %s", c.name, res.Code, res.Body.String())
			} else if got := getSheet(t, srv, cookie, job.ID).Filename; got != c.filename {
				t.Errorf("%s: filename %q, want %q", c.name, got, c.filename)
			}
			continue
		}
		if res.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", c.name, res.Code, res.Body.String())
			continue
		}
		if got := errorText(t, res); got != "Choose a brochure file" {
			t.Errorf("%s: error %q, want %q", c.name, got, "Choose a brochure file")
		}
		assertNothingAttached(t, srv, cookie, job.ID, dir)
	}

	// A body with no file part at all.
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.WriteField("title", "not a file"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	res := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/file", cookie, &buf, writer.FormDataContentType())
	if res.Code != http.StatusBadRequest {
		t.Fatalf("no file part: status %d, want 400: %s", res.Code, res.Body.String())
	}
	if got := errorText(t, res); got != "Choose a brochure file" {
		t.Fatalf("no file part: error %q, want %q", got, "Choose a brochure file")
	}
	assertNothingAttached(t, srv, cookie, job.ID, dir)
}

func TestSimultaneousUploadsAttachOneFile(t *testing.T) {
	t.Parallel()
	srv, dir := newSlowServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	userID := accountID(t, srv, cookie)
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

	const uploads = 8
	results := make([]*httptest.ResponseRecorder, uploads)
	var ready, wg sync.WaitGroup
	gate := make(chan struct{})
	for i := range results {
		ready.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			<-gate
			results[i] = postFile(t, srv, cookie, job.ID, fmt.Sprintf("file-%d.txt", i), []byte(fmt.Sprintf("content %d\n", i)), "text/plain")
		}()
	}
	ready.Wait()
	close(gate)
	wg.Wait()

	accepted := 0
	for i, res := range results {
		switch res.Code {
		case http.StatusOK:
			accepted++
		case http.StatusBadRequest:
		default:
			t.Errorf("upload %d: status %d: %s", i, res.Code, res.Body.String())
		}
	}
	if accepted != 1 {
		t.Fatalf("%d uploads accepted, want 1", accepted)
	}
	name := getSheet(t, srv, cookie, job.ID).Filename
	var i int
	if _, err := fmt.Sscanf(name, "file-%d.txt", &i); err != nil {
		t.Fatalf("stored filename %q: %v", name, err)
	}
	stored, err := os.ReadFile(filepath.Join(dir, userID, job.ID, "body"))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("content %d\n", i); string(stored) != want {
		t.Fatalf("stored %q for filename %q, want %q", stored, name, want)
	}
	if files := filesUnder(t, dir); len(files) != 1 {
		t.Fatalf("files under the data dir are %v, want one", files)
	}
}

func TestAnUploadThatCannotBeSavedLeavesNoFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := newMem()
	srv := New(store, dir, "")
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	// The same accounts and sheets behind a store that can no longer write.
	broken := New(brokenStore{store}, dir, "")

	res := postFile(t, broken, cookie, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", res.Code, res.Body.String())
	}
	assertNothingAttached(t, srv, cookie, job.ID, dir)
}

func TestAnUploadRequestHasALimitOfItsOwn(t *testing.T) {
	t.Parallel()
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

	// A small, valid file behind 3 MiB of other form data: the file is within
	// its limit, the request is not.
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.WriteField("padding", strings.Repeat("x", 3<<20)); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", "brochure.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("small\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	res := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/file", cookie, &buf, writer.FormDataContentType())
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.Code, res.Body.String())
	}
	assertNothingAttached(t, srv, cookie, job.ID, dir)
}

func TestDownloadReturnsTheStoredBytes(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	cases := []struct {
		name        string
		filename    string
		uploadType  string
		data        []byte
		contentType string
		disposition string
	}{
		{"pdf named .txt", "notes.txt", "text/plain", []byte("%PDF-1.4\n%binary \xff\xfe"), "application/pdf", "attachment"},
		{"text named .pdf", "scan.pdf", "application/pdf", []byte("plain words\n"), "text/plain", "attachment"},
		{"a name with directories", "../../escaped.txt", "text/plain", []byte("plain\n"), "text/plain", "attachment"},
	}
	for _, c := range cases {
		job := createSheet(t, srv, cookie, "Sheet", "10 Market Street")
		uploadFile(t, srv, cookie, job.ID, c.filename, c.data, c.uploadType)

		res := call(srv, http.MethodGet, "/api/jobs/"+job.ID+"/file", cookie, nil, "")
		if res.Code != http.StatusOK {
			t.Errorf("%s: status %d: %s", c.name, res.Code, res.Body.String())
			continue
		}
		if !bytes.Equal(res.Body.Bytes(), c.data) {
			t.Errorf("%s: body %q, want %q", c.name, res.Body.Bytes(), c.data)
		}
		if got := res.Header().Get("Content-Type"); !strings.HasPrefix(got, c.contentType) {
			t.Errorf("%s: Content-Type %q, want %q", c.name, got, c.contentType)
		}
		if got := res.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options %q, want nosniff", c.name, got)
		}
		got := res.Header().Get("Content-Disposition")
		if !strings.HasPrefix(got, c.disposition) {
			t.Errorf("%s: Content-Disposition %q, want it to start with %q", c.name, got, c.disposition)
		}
		if c.filename == "../../escaped.txt" {
			if !strings.Contains(got, "escaped.txt") || strings.Contains(got, "..") || strings.Contains(got, "/") {
				t.Errorf("%s: Content-Disposition %q should carry only the base name escaped.txt", c.name, got)
			}
		}
	}
}

func TestDownloadWithoutABrochureIsNotFound(t *testing.T) {
	t.Parallel()
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	userID := accountID(t, srv, cookie)
	bare := createSheet(t, srv, cookie, "Bare", "10 Market Street")
	gone := readySheet(t, srv, cookie, "Gone")

	if res := call(srv, http.MethodGet, "/api/jobs/"+bare.ID+"/file", cookie, nil, ""); res.Code != http.StatusNotFound {
		t.Errorf("sheet without a brochure: status %d, want 404", res.Code)
	}
	if err := os.Remove(filepath.Join(dir, userID, gone.ID, "body")); err != nil {
		t.Fatal(err)
	}
	res := call(srv, http.MethodGet, "/api/jobs/"+gone.ID+"/file", cookie, nil, "")
	if res.Code != http.StatusNotFound {
		t.Errorf("sheet whose file is gone: status %d, want 404", res.Code)
	}
	if got := errorText(t, res); got != "Not found" {
		t.Errorf("error %q, want %q", got, "Not found")
	}
}

// assertNothingAttached checks that a rejected upload left no trace: the data
// directory is empty and the sheet still has no brochure.
func assertNothingAttached(t *testing.T, srv *Server, cookie, id, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the data dir holds %d entries after a rejected upload, want none", len(entries))
	}
	if got := getSheet(t, srv, cookie, id).Filename; got != "" {
		t.Errorf("filename %q after a rejected upload, want none", got)
	}
}

func TestListHidesOtherUsersJobs(t *testing.T) {
	srv, _ := newTestServer(t)
	ada := signUp(t, srv, "ada@example.com")
	grace := signUp(t, srv, "grace@example.com")
	job := createSheet(t, srv, ada, "Market loft", "10 Market Street")

	res := call(srv, http.MethodGet, "/api/jobs", grace, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	var jobs []board.Job
	if err := json.Unmarshal(res.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	for _, item := range jobs {
		if item.ID == job.ID {
			t.Fatal("another account can see this sheet in the list")
		}
	}
}

func TestStartRequiresASession(t *testing.T) {
	srv, _ := newTestServer(t)
	res := call(srv, http.MethodPost, "/api/jobs/"+board.NewID()+"/start", "", nil, "")
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", res.Code)
	}
}

func TestSheetsOfAnotherAccountAreNotFound(t *testing.T) {
	t.Parallel()
	srv, dir := newTestServer(t)
	ada := signUp(t, srv, "ada@example.com")
	grace := signUp(t, srv, "grace@example.com")
	brochure := []byte("%PDF-1.4\n")
	job := createSheet(t, srv, ada, "Market loft", "10 Market Street")
	uploadFile(t, srv, ada, job.ID, "brochure.pdf", brochure, "application/pdf")
	before := filesUnder(t, dir)
	unknown := board.NewID()

	routes := []struct {
		name string
		call func(id string) *httptest.ResponseRecorder
	}{
		{"get the sheet", func(id string) *httptest.ResponseRecorder {
			return call(srv, http.MethodGet, "/api/jobs/"+id, grace, nil, "")
		}},
		{"start the sheet", func(id string) *httptest.ResponseRecorder {
			return call(srv, http.MethodPost, "/api/jobs/"+id+"/start", grace, nil, "")
		}},
		{"attach a brochure", func(id string) *httptest.ResponseRecorder {
			return postFile(t, srv, grace, id, "notes.txt", []byte("not yours"), "text/plain")
		}},
		{"download the brochure", func(id string) *httptest.ResponseRecorder {
			return call(srv, http.MethodGet, "/api/jobs/"+id+"/file", grace, nil, "")
		}},
	}
	for _, route := range routes {
		missing := route.call(unknown)
		if missing.Code != http.StatusNotFound {
			t.Errorf("%s, unknown sheet: status %d, want 404", route.name, missing.Code)
		}
		theirs := route.call(job.ID)
		if theirs.Code != http.StatusNotFound {
			t.Errorf("%s, another account's sheet: status %d, want 404", route.name, theirs.Code)
		}
		// The answer is the one an unknown id gets, so the id alone gives nothing away.
		if theirs.Body.String() != missing.Body.String() {
			t.Errorf("%s: body %q for another account's sheet, %q for an unknown one",
				route.name, theirs.Body.String(), missing.Body.String())
		}
	}

	// Nothing the other account tried reached the owner's sheet or its files.
	got := getSheet(t, srv, ada, job.ID)
	if got.Status != "draft" || got.RunID != "" || got.Filename != "brochure.pdf" {
		t.Errorf("the owner's sheet changed: status %q, runId %q, filename %q", got.Status, got.RunID, got.Filename)
	}
	if after := filesUnder(t, dir); !slices.Equal(after, before) {
		t.Errorf("files under the data dir are %v, were %v", after, before)
	}
	own := call(srv, http.MethodGet, "/api/jobs/"+job.ID+"/file", ada, nil, "")
	if own.Code != http.StatusOK || !bytes.Equal(own.Body.Bytes(), brochure) {
		t.Errorf("the owner's download: status %d, body %q", own.Code, own.Body.String())
	}
}

func TestAccountLocksAdmitOneRequestPerAccount(t *testing.T) {
	t.Parallel()
	var locks accountLocks
	const requests = 64
	inside, visits := 0, 0
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := locks.lock("ada")
			defer unlock()
			inside++
			if inside != 1 {
				t.Errorf("%d requests hold the account's lock at once", inside)
			}
			visits++
			inside--
		}()
	}
	wg.Wait()
	if visits != requests {
		t.Fatalf("%d of %d requests got the lock", visits, requests)
	}
}

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	return New(newMem(), dir, ""), dir
}

// newSlowServer is newTestServer on a store that pauses after every read.
func newSlowServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	return New(slowStore{newMem()}, dir, ""), dir
}

// slowStore pauses after every read. Two requests that decide on the same
// read then always collide unless something keeps them apart, so a test for
// simultaneous requests fails every time the guard is missing, not only when
// the scheduler happens to interleave them.
type slowStore struct{ board.Store }

func (s slowStore) Job(ctx context.Context, id string) (board.Job, error) {
	job, err := s.Store.Job(ctx, id)
	time.Sleep(2 * time.Millisecond)
	return job, err
}

func (s slowStore) ListJobs(ctx context.Context, userID string) ([]board.Job, error) {
	jobs, err := s.Store.ListJobs(ctx, userID)
	time.Sleep(2 * time.Millisecond)
	return jobs, err
}

// brokenStore refuses every update, like a database that has gone away.
type brokenStore struct{ board.Store }

func (brokenStore) UpdateJob(context.Context, board.Job) error {
	return errors.New("the database is down")
}

func signUp(t *testing.T, srv *Server, email string) string {
	t.Helper()
	body := strings.NewReader(`{"email":"` + email + `","password":"password123"}`)
	res := call(srv, http.MethodPost, "/api/register", "", body, "application/json")
	if res.Code != http.StatusCreated {
		t.Fatalf("register %d: %s", res.Code, res.Body.String())
	}
	for _, cookie := range res.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie.Value
		}
	}
	t.Fatal("register did not set a session")
	return ""
}

func createSheet(t *testing.T, srv *Server, cookie, title, address string) board.Job {
	t.Helper()
	body := strings.NewReader(`{"title":"` + title + `","address":"` + address + `"}`)
	res := call(srv, http.MethodPost, "/api/jobs", cookie, body, "application/json")
	if res.Code != http.StatusCreated {
		t.Fatalf("create %d: %s", res.Code, res.Body.String())
	}
	return decodeJob(t, res)
}

func uploadFile(t *testing.T, srv *Server, cookie, id, name string, data []byte, contentType string) {
	t.Helper()
	res := postFile(t, srv, cookie, id, name, data, contentType)
	if res.Code != http.StatusOK {
		t.Fatalf("upload %d: %s", res.Code, res.Body.String())
	}
}

// postFile sends one brochure upload and returns the answer, whatever it is.
func postFile(t *testing.T, srv *Server, cookie, id, name string, data []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, name))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return call(srv, http.MethodPost, "/api/jobs/"+id+"/file", cookie, &buf, writer.FormDataContentType())
}

func getSheet(t *testing.T, srv *Server, cookie, id string) board.Job {
	t.Helper()
	res := call(srv, http.MethodGet, "/api/jobs/"+id, cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("get sheet %d: %s", res.Code, res.Body.String())
	}
	return decodeJob(t, res)
}

// readySheet creates a draft with a brochure attached, ready to start.
func readySheet(t *testing.T, srv *Server, cookie, title string) board.Job {
	t.Helper()
	job := createSheet(t, srv, cookie, title, "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")
	return job
}

func startSheet(srv *Server, cookie, id string) *httptest.ResponseRecorder {
	return call(srv, http.MethodPost, "/api/jobs/"+id+"/start", cookie, nil, "")
}

// startOK starts a sheet that must start and returns the answer.
func startOK(t *testing.T, srv *Server, cookie, id string) board.Job {
	t.Helper()
	res := startSheet(srv, cookie, id)
	if res.Code != http.StatusOK {
		t.Fatalf("start %d: %s", res.Code, res.Body.String())
	}
	return decodeJob(t, res)
}

func listSheets(t *testing.T, srv *Server, cookie string) []board.Job {
	t.Helper()
	res := call(srv, http.MethodGet, "/api/jobs", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("list %d: %s", res.Code, res.Body.String())
	}
	var jobs []board.Job
	if err := json.Unmarshal(res.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	return jobs
}

// errorText returns the message of a JSON error body that holds nothing else.
func errorText(t *testing.T, res *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %q: %v", res.Body.String(), err)
	}
	if len(body) != 1 {
		t.Fatalf("error body %q, want only an error", res.Body.String())
	}
	text, ok := body["error"]
	if !ok {
		t.Fatalf("error body %q has no error", res.Body.String())
	}
	return text
}

// setSheet changes a sheet behind the API's back, as the worker would, and
// returns what it stored.
func setSheet(t *testing.T, srv *Server, id string, change func(*board.Job)) board.Job {
	t.Helper()
	job, err := srv.store.Job(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	change(&job)
	if err := srv.store.UpdateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

// sameRun reports whether two views of a sheet agree on its run.
func sameRun(a, b board.Job) bool {
	return a.ID == b.ID && a.Status == b.Status && a.Error == b.Error &&
		a.RunID == b.RunID && slices.Equal(a.Checklist, b.Checklist)
}

// accountID asks the API who the session belongs to.
func accountID(t *testing.T, srv *Server, cookie string) string {
	t.Helper()
	res := call(srv, http.MethodGet, "/api/me", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("me %d: %s", res.Code, res.Body.String())
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.ID == "" {
		t.Fatal("me has no id")
	}
	return me.ID
}

// filesUnder lists every file below root, relative to it, in walk order.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	files := []string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func decodeJob(t *testing.T, res *httptest.ResponseRecorder) board.Job {
	t.Helper()
	var job board.Job
	if err := json.Unmarshal(res.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	return job
}

func call(srv *Server, method, path, cookie string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	return res
}

type mem struct {
	mu       sync.Mutex
	users    map[string]board.User
	byEmail  map[string]string
	sessions map[string]memSession
	jobs     map[string]board.Job
}

type memSession struct {
	userID  string
	expires time.Time
}

func newMem() *mem {
	return &mem{
		users:    map[string]board.User{},
		byEmail:  map[string]string{},
		sessions: map[string]memSession{},
		jobs:     map[string]board.Job{},
	}
}

func (m *mem) CreateUser(_ context.Context, email, passwordHash string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byEmail[email]; ok {
		return board.User{}, board.ErrDuplicate
	}
	user := board.User{ID: board.NewID(), Email: email, PasswordHash: passwordHash}
	m.users[user.ID] = user
	m.byEmail[email] = user.ID
	return user, nil
}

func (m *mem) UserByEmail(_ context.Context, email string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byEmail[email]
	if !ok {
		return board.User{}, board.ErrNotFound
	}
	return m.users[id], nil
}

func (m *mem) CreateSession(_ context.Context, userID, token string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[token] = memSession{userID: userID, expires: expires}
	return nil
}

func (m *mem) DeleteSession(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
	return nil
}

func (m *mem) UserByToken(_ context.Context, token string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[token]
	if !ok || time.Now().After(session.expires) {
		return board.User{}, board.ErrNotFound
	}
	user, ok := m.users[session.userID]
	if !ok {
		return board.User{}, board.ErrNotFound
	}
	return user, nil
}

func (m *mem) ListJobs(_ context.Context, userID string) ([]board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var jobs []board.Job
	for _, job := range m.jobs {
		if job.UserID == userID {
			jobs = append(jobs, cloneJob(job))
		}
	}
	if jobs == nil {
		jobs = []board.Job{}
	}
	return jobs, nil
}

func (m *mem) CreateJob(_ context.Context, job board.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[job.ID] = cloneJob(job)
	return nil
}

func (m *mem) Job(_ context.Context, id string) (board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return board.Job{}, board.ErrNotFound
	}
	return cloneJob(job), nil
}

func (m *mem) UpdateJob(_ context.Context, job board.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[job.ID]; !ok {
		return board.ErrNotFound
	}
	m.jobs[job.ID] = cloneJob(job)
	return nil
}

func cloneJob(job board.Job) board.Job {
	job.Checklist = append([]board.Step(nil), job.Checklist...)
	return job
}

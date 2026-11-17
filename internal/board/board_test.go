package board

import (
	"path/filepath"
	"testing"
)

// The README fixes these texts; the tests spell them out so that a typo in a
// constant cannot go unnoticed.
func TestChecklistMatchesTheReadme(t *testing.T) {
	want := []string{"Reading the brochure", "Checking the address", "Publishing the sheet"}
	got := NewChecklist()
	if len(got) != len(want) {
		t.Fatalf("%d steps, want %d", len(got), len(want))
	}
	for i, step := range got {
		if step.Label != want[i] {
			t.Errorf("step %d is %q, want %q", i, step.Label, want[i])
		}
		if step.State != "pending" {
			t.Errorf("step %q starts as %q, want pending", step.Label, step.State)
		}
	}
}

func TestStatusAndStepTextsMatchTheReadme(t *testing.T) {
	texts := map[string]string{
		StatusDraft:     "draft",
		StatusRunning:   "running",
		StatusSuccess:   "success",
		StatusFailed:    "failed",
		StepPending:     "pending",
		StepDone:        "done",
		BrochureMissing: "Brochure is missing",
	}
	for got, want := range texts {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestBrochurePathIsUserThenSheetThenBody(t *testing.T) {
	got := BrochurePath("/data", "user1", "sheet1")
	want := filepath.FromSlash("/data/user1/sheet1/body")
	if got != want {
		t.Fatalf("path %q, want %q", got, want)
	}
}

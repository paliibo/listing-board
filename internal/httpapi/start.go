package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"listingboard/internal/board"
)

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	// Two starts of one account must not both decide on the same read, or one
	// draft could get two run ids and an account two running sheets.
	unlock := s.locks.lock(user.ID)
	defer unlock()
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	for retried := false; ; retried = true {
		// Only a draft is started. A running sheet keeps its run id, and a
		// finished one stays as the worker left it.
		if job.Status != board.StatusDraft {
			writeJSON(w, http.StatusOK, job)
			return
		}
		if strings.TrimSpace(job.Filename) == "" {
			writeErr(w, http.StatusBadRequest, "Attach a brochure first")
			return
		}
		busy, err := s.hasOtherRunning(r.Context(), user.ID, job.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "Could not start the sheet")
			return
		}
		if busy {
			writeErr(w, http.StatusConflict, "Another sheet is already running")
			return
		}
		next := job
		next.Status = board.StatusRunning
		next.RunID = board.NewID()
		next.Error = ""
		next.Checklist = make([]board.Step, len(job.Checklist))
		for i, step := range job.Checklist {
			next.Checklist[i] = board.Step{Label: step.Label, State: board.StepPending}
		}
		next.UpdatedAt = time.Now().UTC()
		err = s.store.UpdateJob(r.Context(), next)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, next)
			return
		case errors.Is(err, board.ErrDuplicate):
			// The store saw another running sheet of this account that our
			// lock could not: another API process started it.
			writeErr(w, http.StatusConflict, "Another sheet is already running")
			return
		case errors.Is(err, board.ErrNotFound) && !retried:
			// The sheet left draft or vanished since we read it. Answer from
			// what it is now, once; s.load answers 404 if it is gone.
			job, ok = s.load(w, r, user)
			if !ok {
				return
			}
		default:
			writeErr(w, http.StatusInternalServerError, "Could not start the sheet")
			return
		}
	}
}

// hasOtherRunning reports whether the account has a running sheet other than id.
func (s *Server) hasOtherRunning(ctx context.Context, userID, id string) (bool, error) {
	jobs, err := s.store.ListJobs(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, other := range jobs {
		if other.ID != id && other.Status == board.StatusRunning {
			return true, nil
		}
	}
	return false, nil
}

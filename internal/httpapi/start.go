package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"listingboard/internal/board"
)

const anotherRunning = "Another sheet is already running"

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
		writeErr(w, http.StatusConflict, anotherRunning)
		return
	}
	// The run id is chosen here and nowhere else. The steps stay as a draft
	// has them, pending: the checklist is the worker's.
	job.Status = board.StatusRunning
	job.RunID = board.NewID()
	job.UpdatedAt = time.Now().UTC()
	err = s.store.UpdateJob(r.Context(), job)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, job)
	case errors.Is(err, board.ErrDuplicate):
		// The store saw a running sheet of this account that the check above
		// could not: another API process started it in between.
		writeErr(w, http.StatusConflict, anotherRunning)
	case errors.Is(err, board.ErrNotFound):
		// The sheet left draft, or vanished, after it was read: another API
		// process got to it first. Answer from what the sheet is now.
		current, ok := s.load(w, r, user)
		if !ok {
			return
		}
		if current.Status == board.StatusDraft {
			writeErr(w, http.StatusInternalServerError, "Could not start the sheet")
			return
		}
		writeJSON(w, http.StatusOK, current)
	default:
		writeErr(w, http.StatusInternalServerError, "Could not start the sheet")
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

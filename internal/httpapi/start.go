package httpapi

import (
	"net/http"
	"strings"
	"time"

	"listingboard/internal/board"
)

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r); !ok {
		return
	}
	job, ok := s.load(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(job.Filename) == "" {
		writeErr(w, http.StatusBadRequest, "Attach a brochure first")
		return
	}
	job.Status = "success"
	job.RunID = board.NewID()
	job.Error = ""
	for i := range job.Checklist {
		job.Checklist[i].State = "done"
	}
	job.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateJob(r.Context(), job); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start the sheet")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

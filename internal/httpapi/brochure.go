package httpapi

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r); !ok {
		return
	}
	job, ok := s.load(w, r)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "Brochure upload was incomplete")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Choose a brochure file")
		return
	}
	defer file.Close()
	name := filenameFromHeader(header)
	if strings.TrimSpace(name) == "" || !headerAllows(name, header.Header.Get("Content-Type")) {
		writeErr(w, http.StatusBadRequest, "Brochure must be a PDF or a text file")
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, 8<<20+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Brochure upload was incomplete")
		return
	}
	if len(data) > 8<<20 {
		writeErr(w, http.StatusBadRequest, "Brochure is too large")
		return
	}
	dir := filepath.Join(s.dataDir, job.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}
	job.Filename = name
	job.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateJob(r.Context(), job); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r); !ok {
		return
	}
	job, ok := s.load(w, r)
	if !ok {
		return
	}
	if job.Filename == "" {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	f, err := os.Open(filepath.Join(s.dataDir, job.ID, job.Filename))
	if err != nil {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Disposition", "attachment; filename="+strconvQuote(filepath.Base(job.Filename)))
	http.ServeContent(w, r, filepath.Base(job.Filename), job.UpdatedAt, f)
}

func filenameFromHeader(header *multipart.FileHeader) string {
	_, params, err := mime.ParseMediaType(header.Header.Get("Content-Disposition"))
	if err == nil {
		if name := strings.TrimSpace(params["filename"]); name != "" {
			return name
		}
	}
	return header.Filename
}

func headerAllows(name, contentType string) bool {
	ct := strings.ToLower(contentType)
	lower := strings.ToLower(name)
	if strings.Contains(ct, "pdf") || strings.Contains(ct, "text/plain") {
		return true
	}
	return strings.HasSuffix(lower, ".pdf") || strings.HasSuffix(lower, ".txt")
}

func strconvQuote(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}

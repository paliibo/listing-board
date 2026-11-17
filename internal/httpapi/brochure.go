package httpapi

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"listingboard/internal/board"
)

const (
	maxBrochure      = 1 << 20
	maxUploadBody    = 2 << 20 // the multipart envelope adds bytes to the file
	maxFilenameRunes = 255     // the longest name the common file systems allow
)

var pdfMagic = []byte("%PDF-")

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	if msg := notAttachable(job); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}

	// Stream the body: ParseMultipartForm would read all of it and spill large
	// files into temporary files, which a rejected upload must not leave behind.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	reader, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Brochure upload was incomplete")
		return
	}
	var part *multipart.Part
	var name string
	for part == nil {
		p, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "Choose a brochure file")
			return
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "Brochure upload was incomplete")
			return
		}
		if p.FormName() != "file" {
			continue
		}
		// part.FileName() would drop the directories; the client's name is kept as sent.
		_, params, err := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
		if err != nil {
			continue
		}
		if filename, has := params["filename"]; has {
			part, name = p, filename
		}
	}
	if strings.TrimSpace(name) == "" {
		writeErr(w, http.StatusBadRequest, "Choose a brochure file")
		return
	}
	if !storableFilename(name) {
		writeErr(w, http.StatusBadRequest, "Brochure file name is not valid")
		return
	}

	data, err := io.ReadAll(io.LimitReader(part, maxBrochure+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Brochure upload was incomplete")
		return
	}
	if len(data) > maxBrochure {
		writeErr(w, http.StatusBadRequest, "Brochure is too large")
		return
	}
	if !isPDF(data) && !isText(data) {
		writeErr(w, http.StatusBadRequest, "Brochure must be a PDF or a text file")
		return
	}

	unlock := s.locks.lock(user.ID)
	defer unlock()
	// Another request may have attached a file or started the sheet while this
	// one was reading its body.
	job, ok = s.load(w, r, user)
	if !ok {
		return
	}
	if msg := notAttachable(job); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	path := board.BrochurePath(s.dataDir, user.ID, job.ID)
	created, err := mkdirAll(filepath.Dir(path))
	if err == nil {
		err = os.WriteFile(path, data, 0o644)
	}
	if err != nil {
		removeBrochure(path, created)
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}
	job.Filename = name
	job.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateJob(r.Context(), job); err != nil {
		// Take the file away only when the sheet is known to have no brochure.
		// An error can also mean that the answer was lost after the row was
		// written; the row then names this file, and removing it would leave
		// a sheet that can only fail.
		if saved, loadErr := s.store.Job(r.Context(), job.ID); loadErr == nil && saved.Filename == "" {
			removeBrochure(path, created)
		}
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	if job.Filename == "" {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	f, err := os.Open(board.BrochurePath(s.dataDir, job.UserID, job.ID))
	if err != nil {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	head := make([]byte, len(pdfMagic))
	n, _ := io.ReadFull(f, head)
	contentType := "text/plain; charset=utf-8"
	if isPDF(head[:n]) {
		contentType = "application/pdf"
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", attachment(job.Filename))
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// notAttachable says why a sheet cannot take a brochure, or "" when it can.
func notAttachable(job board.Job) string {
	if job.Filename != "" {
		return "This sheet already has a brochure"
	}
	if job.Status != board.StatusDraft {
		return "This sheet has already been started"
	}
	return ""
}

// storableFilename reports whether the client's name can be kept as it is.
// Postgres refuses NUL and invalid UTF-8 in a TEXT column, which would
// otherwise surface as a 500. The length cap counts characters, not bytes,
// so it turns away no name a real file can have; like the caps on title and
// address it keeps one sheet from growing the list without bound.
func storableFilename(name string) bool {
	return utf8.ValidString(name) && !strings.ContainsRune(name, 0) &&
		utf8.RuneCountInString(name) <= maxFilenameRunes
}

func isPDF(data []byte) bool {
	return bytes.HasPrefix(data, pdfMagic)
}

func isText(data []byte) bool {
	return len(data) > 0 && !isPDF(data) && utf8.Valid(data) && bytes.IndexByte(data, 0) < 0
}

// attachment builds the Content-Disposition value from the base name of the
// client's file name, whichever separator the client's system uses.
func attachment(name string) string {
	base := name[strings.LastIndexAny(name, `/\`)+1:]
	if base == "" || base == "." || base == ".." {
		base = "brochure"
	}
	value := mime.FormatMediaType("attachment", map[string]string{"filename": base})
	if value == "" {
		return "attachment"
	}
	return value
}

// mkdirAll creates dir and reports which directories this call created, so a
// failed upload can take them away again.
func mkdirAll(dir string) ([]string, error) {
	var created []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		}
		created = append(created, d)
		if d == filepath.Dir(d) {
			break
		}
	}
	return created, os.MkdirAll(dir, 0o755)
}

// removeBrochure undoes a write. Directories go with plain Remove, which
// refuses a directory that is not empty, so nothing else is ever deleted.
func removeBrochure(path string, created []string) {
	_ = os.Remove(path)
	for _, d := range created {
		_ = os.Remove(d)
	}
}

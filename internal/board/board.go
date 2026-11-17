package board

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"time"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrDuplicate = errors.New("duplicate")
)

// Sheet statuses. The API moves a draft to running; only the worker moves a
// running sheet on to success or failed.
const (
	StatusDraft   = "draft"
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusFailed  = "failed"
)

// Checklist step states.
const (
	StepPending = "pending"
	StepDone    = "done"
)

// BrochureMissing is the error a sheet carries once its brochure cannot be read.
const BrochureMissing = "Brochure is missing"

type User struct {
	ID           string
	Email        string
	PasswordHash string
}

type Step struct {
	Label string `json:"label"`
	State string `json:"state"`
}

type Job struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	Title     string    `json:"title"`
	Address   string    `json:"address"`
	Status    string    `json:"status"`
	Error     string    `json:"error"`
	RunID     string    `json:"runId"`
	Filename  string    `json:"filename"`
	Checklist []Step    `json:"checklist"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func NewChecklist() []Step {
	return []Step{
		{Label: "Reading the brochure", State: StepPending},
		{Label: "Checking the address", State: StepPending},
		{Label: "Publishing the sheet", State: StepPending},
	}
}

// BrochurePath is where the bytes of a sheet's brochure live. The API writes
// the file and the worker reads it, so both build the path here. The name the
// client gave the file is never part of it.
func BrochurePath(dataDir, userID, jobID string) string {
	return filepath.Join(dataDir, userID, jobID, "body")
}

func NewID() string {
	return randomHex(16)
}

func NewToken() string {
	return randomHex(32)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

// Store is the persistence used by the HTTP API.
type Store interface {
	CreateUser(ctx context.Context, email, passwordHash string) (User, error)
	UserByEmail(ctx context.Context, email string) (User, error)
	CreateSession(ctx context.Context, userID, token string, expires time.Time) error
	DeleteSession(ctx context.Context, token string) error
	UserByToken(ctx context.Context, token string) (User, error)
	ListJobs(ctx context.Context, userID string) ([]Job, error)
	CreateJob(ctx context.Context, job Job) error
	Job(ctx context.Context, id string) (Job, error)
	UpdateJob(ctx context.Context, job Job) error
}

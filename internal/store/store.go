package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"listingboard/internal/board"
)

type Store struct {
	db *sql.DB
}

func Open(dsn string) (*Store, error) {
	return open(context.Background(), dsn)
}

// Wait opens the store, retrying every 500 ms until it succeeds, patience has
// passed, or ctx is done; then it returns the last error. Both binaries start
// alongside Postgres, which may not accept connections yet.
func Wait(ctx context.Context, dsn string, patience time.Duration) (*Store, error) {
	ctx, cancel := context.WithTimeout(ctx, patience)
	defer cancel()
	var last error
	for {
		st, err := open(ctx, dsn)
		if err == nil {
			return st, nil
		}
		// An attempt cut short by the deadline only says "deadline exceeded";
		// the attempt before it says why Postgres was not reachable.
		if last == nil || ctx.Err() == nil {
			last = err
		}
		select {
		case <-ctx.Done():
			return nil, last
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// open takes ctx so that Wait can abandon an attempt that hangs on connect.
func open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// migrateLock is the advisory lock key that serialises migrations. The API
// and the worker start together and both migrate; CREATE ... IF NOT EXISTS is
// not safe against a concurrent creator and fails with a unique violation on
// the catalog, so each process waits for the other to commit first.
const migrateLock = 7_301_964_117

func (s *Store) migrate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrateLock)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (board.User, error) {
	user := board.User{ID: board.NewID(), Email: email, PasswordHash: passwordHash}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, created_at)
		VALUES ($1, $2, $3, $4)
	`, user.ID, user.Email, user.PasswordHash, time.Now().UTC())
	if isUnique(err) {
		return board.User{}, board.ErrDuplicate
	}
	if err != nil {
		return board.User{}, err
	}
	return user, nil
}

func (s *Store) UserByEmail(ctx context.Context, email string) (board.User, error) {
	var user board.User
	err := s.db.QueryRowContext(ctx, `
		SELECT id, email, password_hash FROM users WHERE email = $1
	`, email).Scan(&user.ID, &user.Email, &user.PasswordHash)
	return user, mapNotFound(err)
}

func (s *Store) CreateSession(ctx context.Context, userID, token string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (token, user_id, expires_at) VALUES ($1, $2, $3)
	`, token, userID, expires.UTC())
	return err
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = $1`, token)
	return err
}

func (s *Store) UserByToken(ctx context.Context, token string) (board.User, error) {
	var user board.User
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.email
		FROM users u
		JOIN sessions s ON s.user_id = u.id
		WHERE s.token = $1 AND s.expires_at > now()
	`, token).Scan(&user.ID, &user.Email)
	return user, mapNotFound(err)
}

func (s *Store) ListJobs(ctx context.Context, userID string) ([]board.Job, error) {
	rows, err := s.db.QueryContext(ctx, jobSelect+` WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []board.Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if jobs == nil {
		jobs = []board.Job{}
	}
	return jobs, rows.Err()
}

func (s *Store) CreateJob(ctx context.Context, job board.Job) error {
	raw, err := json.Marshal(job.Checklist)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO jobs (
			id, user_id, title, address, status, error, run_id, filename, checklist, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, job.ID, job.UserID, job.Title, job.Address, job.Status, job.Error, job.RunID, job.Filename, string(raw), job.CreatedAt.UTC(), job.UpdatedAt.UTC())
	return err
}

func (s *Store) Job(ctx context.Context, id string) (board.Job, error) {
	row := s.db.QueryRowContext(ctx, jobSelect+` WHERE id = $1`, id)
	job, err := scanJob(row)
	return job, mapNotFound(err)
}

func (s *Store) UpdateJob(ctx context.Context, job board.Job) error {
	raw, err := json.Marshal(job.Checklist)
	if err != nil {
		return err
	}
	// The API only ever changes drafts (attach a brochure, start). Once a sheet
	// is running it belongs to the worker, so a stale API write must lose
	// (ErrNotFound) instead of overwriting the worker's progress.
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET status = $1, error = $2, run_id = $3, filename = $4, checklist = $5, updated_at = $6
		WHERE id = $7 AND status = 'draft'
	`, job.Status, job.Error, job.RunID, job.Filename, string(raw), job.UpdatedAt.UTC(), job.ID)
	if isUnique(err) {
		// jobs_one_running_per_user: the account already has a running sheet.
		return board.ErrDuplicate
	}
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return board.ErrNotFound
	}
	return nil
}

// RunningJobs returns every running sheet, the one waiting longest first, so
// that no sheet is starved when the worker falls behind.
func (s *Store) RunningJobs(ctx context.Context) ([]board.Job, error) {
	rows, err := s.db.QueryContext(ctx, jobSelect+` WHERE status = 'running' ORDER BY updated_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []board.Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// SaveProgress writes the worker's fields of next, but only while the row is
// still the running sheet the worker read as seen. Another worker instance may
// have moved it on since, and two writers deciding on the same read must not
// both win; the loser gets false and skips the sheet. run_id, filename, title
// and address are never written here: they belong to the API.
func (s *Store) SaveProgress(ctx context.Context, seen, next board.Job) (bool, error) {
	raw, err := json.Marshal(next.Checklist)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET status = $1, error = $2, checklist = $3, updated_at = $4
		WHERE id = $5 AND status = 'running' AND updated_at = $6
	`, next.Status, next.Error, string(raw), next.UpdatedAt.UTC(), seen.ID, seen.UpdatedAt.UTC())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

const jobSelect = `
	SELECT id, user_id, title, address, status, error, run_id, filename, checklist, created_at, updated_at
	FROM jobs`

type scanner interface {
	Scan(dest ...any) error
}

func scanJob(row scanner) (board.Job, error) {
	var job board.Job
	var raw string
	err := row.Scan(
		&job.ID, &job.UserID, &job.Title, &job.Address, &job.Status, &job.Error,
		&job.RunID, &job.Filename, &raw, &job.CreatedAt, &job.UpdatedAt,
	)
	if err != nil {
		return board.Job{}, err
	}
	if err := json.Unmarshal([]byte(raw), &job.Checklist); err != nil {
		return board.Job{}, err
	}
	if job.Checklist == nil {
		job.Checklist = []board.Step{}
	}
	return job, nil
}

func mapNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return board.ErrNotFound
	}
	return err
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

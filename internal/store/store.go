package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Account is one connected Antigravity (Google) identity used for search grounding.
type Account struct {
	ID            string
	Email         string
	AccessToken   string
	RefreshToken  string
	ProjectID     string
	ExpiresAt     time.Time // access token expiry
	IsActive      bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	LastError     string
	FailureCount  int
	LastErrorAt   *time.Time
	LastRefreshAt *time.Time
	LastSearchAt  *time.Time
}

// APIKey is a downstream consumer key.
type APIKey struct {
	ID         string
	Label      string
	Prefix     string // first 8 chars of the key, for display
	KeyHash    string // sha256 hex of the full key
	IsActive   bool
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// Usage is one completed search request by a downstream key.
type Usage struct {
	ID        int64
	AccountID string
	APIKeyID  string
	Query     string
	Model     string
	LatencyMS int64
	CreatedAt time.Time
}

var ErrNotFound = errors.New("not found")

// Store wraps the SQLite database.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the SQLite database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS accounts (
	id              TEXT PRIMARY KEY,
	email           TEXT NOT NULL UNIQUE,
	access_token    TEXT NOT NULL DEFAULT '',
	refresh_token   TEXT NOT NULL DEFAULT '',
	project_id      TEXT NOT NULL DEFAULT '',
	expires_at      DATETIME NOT NULL DEFAULT '0001-01-01T00:00:00Z',
	is_active       INTEGER NOT NULL DEFAULT 1,
	created_at      DATETIME NOT NULL,
	updated_at      DATETIME NOT NULL,
	last_error      TEXT NOT NULL DEFAULT '',
	failure_count   INTEGER NOT NULL DEFAULT 0,
	last_error_at   DATETIME,
	last_refresh_at DATETIME,
	last_search_at  DATETIME
);
CREATE TABLE IF NOT EXISTS api_keys (
	id           TEXT PRIMARY KEY,
	label        TEXT NOT NULL,
	prefix       TEXT NOT NULL,
	key_hash     TEXT NOT NULL UNIQUE,
	is_active    INTEGER NOT NULL DEFAULT 1,
	created_at   DATETIME NOT NULL,
	last_used_at DATETIME
);
CREATE TABLE IF NOT EXISTS usage (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	account_id  TEXT NOT NULL,
	api_key_id  TEXT NOT NULL,
	query       TEXT NOT NULL DEFAULT '',
	model       TEXT NOT NULL DEFAULT '',
	latency_ms  INTEGER NOT NULL DEFAULT 0,
	created_at  DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_key_time ON usage(api_key_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_time ON usage(created_at);
`)
	return err
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// NewID returns a fresh random identifier.
func NewID() string { return randomHex(12) }

// NewAPIKeyMaterial returns a new full key ("gsk-..."), its sha256 hex hash, and display prefix.
func NewAPIKeyMaterial() (full, hash, prefix string) {
	full = "gsk-" + randomHex(20)
	return full, sha256Hex(full), full[:8]
}

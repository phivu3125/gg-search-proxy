package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func scanAccount(row interface{ Scan(...any) error }) (*Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Email, &a.AccessToken, &a.RefreshToken, &a.ProjectID,
		&a.ExpiresAt, &a.IsActive, &a.CreatedAt, &a.UpdatedAt,
		&a.LastError, &a.FailureCount, &a.LastErrorAt, &a.LastRefreshAt, &a.LastSearchAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

const accountCols = `id, email, access_token, refresh_token, project_id, expires_at, is_active, created_at, updated_at, last_error, failure_count, last_error_at, last_refresh_at, last_search_at`

// UpsertAccount inserts a new account or updates tokens for the same email.
func (s *Store) UpsertAccount(ctx context.Context, a *Account) error {
	a.UpdatedAt = time.Now()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO accounts (id, email, access_token, refresh_token, project_id, expires_at, is_active, created_at, updated_at, last_error, failure_count, last_error_at, last_refresh_at, last_search_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(email) DO UPDATE SET
	access_token=excluded.access_token,
	refresh_token=excluded.refresh_token,
	project_id=excluded.project_id,
	expires_at=excluded.expires_at,
	is_active=excluded.is_active,
	updated_at=excluded.updated_at,
	last_error=excluded.last_error,
	failure_count=excluded.failure_count,
	last_error_at=excluded.last_error_at,
	last_refresh_at=excluded.last_refresh_at,
	last_search_at=excluded.last_search_at`,
		a.ID, a.Email, a.AccessToken, a.RefreshToken, a.ProjectID, a.ExpiresAt, boolInt(a.IsActive),
		a.CreatedAt, a.UpdatedAt, a.LastError, a.FailureCount, a.LastErrorAt, a.LastRefreshAt, a.LastSearchAt)
	return err
}

// ListAccounts returns all accounts ordered by email.
func (s *Store) ListAccounts(ctx context.Context) ([]*Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountCols+` FROM accounts ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAccount fetches one account by id.
func (s *Store) GetAccount(ctx context.Context, id string) (*Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = ?`, id))
}

// PickAccount returns the active account with the oldest last_search_at
// (least-recently-used scheduling across accounts).
func (s *Store) PickAccount(ctx context.Context) (*Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, `
SELECT `+accountCols+` FROM accounts
WHERE is_active = 1 AND refresh_token != ''
ORDER BY COALESCE(last_search_at, '0001-01-01T00:00:00Z') ASC, email ASC
LIMIT 1`))
}

// UpdateTokens persists refreshed tokens for an account.
func (s *Store) UpdateTokens(ctx context.Context, id, access string, expiresAt time.Time) error {
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
UPDATE accounts SET access_token=?, expires_at=?, last_refresh_at=?, updated_at=? WHERE id=?`,
		access, expiresAt, now, now, id)
	return err
}

// MarkSearchSuccess records a successful search on an account.
func (s *Store) MarkSearchSuccess(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE accounts SET last_search_at=?, failure_count=0, last_error='', updated_at=? WHERE id=?`,
		time.Now(), time.Now(), id)
	return err
}

// MarkSearchFailure records a failed search on an account.
func (s *Store) MarkSearchFailure(ctx context.Context, id, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE accounts SET last_error=?, failure_count=failure_count+1, last_error_at=?, updated_at=? WHERE id=?`,
		errMsg, time.Now(), time.Now(), id)
	return err
}

// SetAccountActive enables or disables an account.
func (s *Store) SetAccountActive(ctx context.Context, id string, active bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET is_active=?, updated_at=? WHERE id=?`,
		boolInt(active), time.Now(), id)
	return err
}

// DeleteAccount removes an account by id.
func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, id)
	return err
}

// --- API keys ---

func scanAPIKey(row interface{ Scan(...any) error }) (*APIKey, error) {
	var k APIKey
	err := row.Scan(&k.ID, &k.Label, &k.Prefix, &k.KeyHash, &k.IsActive, &k.CreatedAt, &k.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

const keyCols = `id, label, prefix, key_hash, is_active, created_at, last_used_at`

// CreateAPIKey stores a new API key and returns the stored record.
func (s *Store) CreateAPIKey(ctx context.Context, label, fullKey string) (*APIKey, error) {
	k := &APIKey{
		ID:        NewID(),
		Label:     label,
		Prefix:    fullKey[:8],
		KeyHash:   sha256Hex(fullKey),
		IsActive:  true,
		CreatedAt: time.Now(),
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO api_keys (id, label, prefix, key_hash, is_active, created_at) VALUES (?,?,?,?,?,?)`,
		k.ID, k.Label, k.Prefix, k.KeyHash, boolInt(k.IsActive), k.CreatedAt)
	if err != nil {
		return nil, err
	}
	return k, nil
}

// ListAPIKeys returns all keys ordered by creation time.
func (s *Store) ListAPIKeys(ctx context.Context) ([]*APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+keyCols+` FROM api_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// FindAPIKeyByHash looks up a key by its sha256 hash.
func (s *Store) FindAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	return scanAPIKey(s.db.QueryRowContext(ctx, `SELECT `+keyCols+` FROM api_keys WHERE key_hash = ?`, hash))
}

// TouchAPIKey updates last_used_at.
func (s *Store) TouchAPIKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at=? WHERE id=?`, time.Now(), id)
	return err
}

// SetAPIKeyActive enables or disables a key.
func (s *Store) SetAPIKeyActive(ctx context.Context, id string, active bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET is_active=? WHERE id=?`, boolInt(active), id)
	return err
}

// DeleteAPIKey removes a key by id.
func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id=?`, id)
	return err
}

// --- usage ---

// RecordUsage logs one search request.
func (s *Store) RecordUsage(ctx context.Context, accountID, keyID, query, model string, latencyMS int64) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO usage (account_id, api_key_id, query, model, latency_ms, created_at) VALUES (?,?,?,?,?,?)`,
		accountID, keyID, query, model, latencyMS, time.Now())
	return err
}

// UsageSummary returns per-key totals over a lookback window (e.g. "24h", "30d").
func (s *Store) UsageSummary(ctx context.Context, lookback time.Duration) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT api_key_id, COUNT(*) FROM usage WHERE created_at >= ? GROUP BY api_key_id`,
		time.Now().Add(-lookback))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

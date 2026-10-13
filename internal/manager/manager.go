// Package manager coordinates Antigravity accounts: token refresh, scheduling,
// and search with failover across accounts.
package manager

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"ggsearch/internal/antigravity"
	"ggsearch/internal/store"
)

// Manager owns the account pool and upstream client.
type Manager struct {
	store     *store.Store
	client    *antigravity.Client
	log       *slog.Logger
	refreshMu sync.Mutex
}

// New builds a Manager.
func New(st *store.Store, log *slog.Logger) *Manager {
	return &Manager{store: st, client: antigravity.New(), log: log}
}

// ErrNoAccounts means the pool has no usable account.
var ErrNoAccounts = errors.New("no active antigravity accounts configured")

// Login starts the OAuth flow: starts a local callback server, opens the browser,
// exchanges the code, loads the project id, and stores the account.
func (m *Manager) Login(ctx context.Context, browser func(url string) error) (*store.Account, error) {
	state, cb, closeSrv, err := startCallbackServer()
	if err != nil {
		return nil, err
	}
	defer closeSrv()

	redirectURI := fmt.Sprintf("http://localhost:%d/callback", cb.port)
	authURL, err := antigravity.BuildAuthorizeURL(redirectURI, state)
	if err != nil {
		return nil, err
	}
	fmt.Println("\nOpening browser for Antigravity (Google) login...")
	fmt.Println("If it does not open, visit:\n\n" + authURL + "\n")
	if browser != nil {
		if err := browser(authURL); err != nil {
			m.log.Warn("failed to open browser", "err", err)
		}
	}

	select {
	case params := <-cb.ch:
		if params.err != "" {
			return nil, fmt.Errorf("oauth callback error: %s", params.err)
		}
		if params.state != state {
			return nil, fmt.Errorf("oauth state mismatch")
		}
		tokens, err := m.client.ExchangeCode(ctx, params.code, redirectURI)
		if err != nil {
			return nil, fmt.Errorf("exchange code: %w", err)
		}
		email, err := m.client.GetUserEmail(ctx, tokens.AccessToken)
		if err != nil {
			return nil, fmt.Errorf("get user info: %w", err)
		}
		projectID, tierID, err := m.client.LoadProjectID(ctx, tokens.AccessToken)
		if err != nil {
			return nil, fmt.Errorf("loadCodeAssist: %w", err)
		}
		refresh := tokens.RefreshToken
		acc, err := m.upsertAccount(email, tokens.AccessToken, refresh, projectID)
		if err != nil {
			return nil, err
		}
		fmt.Printf("Connected: %s (project %s, tier %s)\n", email, projectID, tierID)
		return acc, nil
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("login timed out after 5 minutes")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) upsertAccount(email, access, refresh, projectID string) (*store.Account, error) {
	existing, err := m.store.ListAccounts(context.Background())
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, a := range existing {
		if a.Email == email {
			a.AccessToken = access
			a.ExpiresAt = now.Add(3599 * time.Second)
			if refresh != "" {
				a.RefreshToken = refresh
			}
			a.ProjectID = projectID
			a.IsActive = true
			a.LastError = ""
			a.FailureCount = 0
			return a, m.store.UpsertAccount(context.Background(), a)
		}
	}
	acc := &store.Account{
		ID:           store.NewID(),
		Email:        email,
		AccessToken:  access,
		RefreshToken: refresh,
		ProjectID:    projectID,
		ExpiresAt:    now.Add(3599 * time.Second),
		IsActive:     true,
		CreatedAt:    now,
	}
	return acc, m.store.UpsertAccount(context.Background(), acc)
}

// ensureAccessToken returns a valid access token, refreshing if needed.
// On refresh failure the account is marked failed and the error returned.
func (m *Manager) ensureAccessToken(ctx context.Context, acc *store.Account) (string, error) {
	// 2-minute safety margin before expiry.
	if acc.AccessToken != "" && time.Until(acc.ExpiresAt) > 2*time.Minute {
		return acc.AccessToken, nil
	}
	if acc.RefreshToken == "" {
		return "", fmt.Errorf("account %s has no refresh token", acc.Email)
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	// Re-check after acquiring the lock (another goroutine may have refreshed).
	fresh, err := m.store.GetAccount(ctx, acc.ID)
	if err == nil && fresh.AccessToken != "" && time.Until(fresh.ExpiresAt) > 2*time.Minute {
		acc.AccessToken = fresh.AccessToken
		acc.ExpiresAt = fresh.ExpiresAt
		return acc.AccessToken, nil
	}
	tokens, err := m.client.RefreshToken(ctx, acc.RefreshToken)
	if err != nil {
		_ = m.store.MarkSearchFailure(ctx, acc.ID, "refresh: "+err.Error())
		return "", fmt.Errorf("refresh token for %s: %w", acc.Email, err)
	}
	newAccess := tokens.AccessToken
	newRefresh := tokens.RefreshToken
	if newRefresh != "" && newRefresh != acc.RefreshToken {
		// Google may rotate the refresh token on refresh.
		acc.RefreshToken = newRefresh
	}
	acc.AccessToken = newAccess
	acc.ExpiresAt = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	if err := m.store.UpdateTokens(ctx, acc.ID, acc.AccessToken, acc.ExpiresAt); err != nil {
		m.log.Warn("persist refreshed tokens failed", "account", acc.Email, "err", err)
	}
	return acc.AccessToken, nil
}

// Search executes a query across the account pool with failover.
// maxAttempts caps how many accounts to try.
func (m *Manager) Search(ctx context.Context, query, model string) (*antigravity.SearchResult, *store.Account, error) {
	accounts, err := m.store.ListAccounts(ctx)
	if err != nil {
		return nil, nil, err
	}
	var active []*store.Account
	for _, a := range accounts {
		if a.IsActive {
			active = append(active, a)
		}
	}
	if len(active) == 0 {
		return nil, nil, ErrNoAccounts
	}
	// Least-recently-used first.
	sortByLRU(active)
	var lastErr error
	for _, acc := range active {
		token, err := m.ensureAccessToken(ctx, acc)
		if err != nil {
			lastErr = err
			continue
		}
		res, err := m.client.Search(ctx, token, acc.ProjectID, antigravity.SearchArgs{Query: query, Model: model})
		if err == nil {
			_ = m.store.MarkSearchSuccess(ctx, acc.ID)
			return res, acc, nil
		}
		_ = m.store.MarkSearchFailure(ctx, acc.ID, err.Error())
		lastErr = err
		if !isRetryable(err) {
			break
		}
	}
	return nil, nil, lastErr
}

func isRetryable(err error) bool {
	var apiErr *antigravity.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case 401, 403:
			return true // may work on another account / after refresh
		case 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	return true // network errors are retryable on another account
}

func sortByLRU(accs []*store.Account) {
	// insertion sort, small n
	for i := 1; i < len(accs); i++ {
		for j := i; j > 0 && lruLess(accs[j], accs[j-1]); j-- {
			accs[j], accs[j-1] = accs[j-1], accs[j]
		}
	}
}

func lruLess(a, b *store.Account) bool {
	at := a.LastSearchAt
	bt := b.LastSearchAt
	if at == nil {
		at = &time.Time{}
	}
	if bt == nil {
		bt = &time.Time{}
	}
	if !at.Equal(*bt) {
		return at.Before(*bt)
	}
	return a.Email < b.Email
}

// Accounts exposes the account list for the admin API.
func (m *Manager) Accounts(ctx context.Context) ([]*store.Account, error) {
	return m.store.ListAccounts(ctx)
}

// Ping verifies one account's credentials by loading its project.
func (m *Manager) Ping(ctx context.Context, id string) (string, error) {
	acc, err := m.store.GetAccount(ctx, id)
	if err != nil {
		return "", err
	}
	token, err := m.ensureAccessToken(ctx, acc)
	if err != nil {
		return "", err
	}
	projectID, _, err := m.client.LoadProjectID(ctx, token)
	return projectID, err
}

// Import9Router imports Antigravity credentials from a 9router SQLite database.
func (m *Manager) Import9Router(ctx context.Context, dbPath string) (imported int, err error) {
	rows, err := read9RouterAntigravityRows(dbPath)
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if r.refreshToken == "" {
			continue
		}
		acc, err := m.upsertAccount(r.email, r.accessToken, r.refreshToken, r.projectID)
		if err != nil {
			return imported, err
		}
		if acc.AccessToken != "" {
			m.log.Info("imported account", "email", acc.Email, "project", acc.ProjectID)
			imported++
		}
	}
	return imported, nil
}

// --- local OAuth callback server ---

type callbackParams struct {
	code  string
	state string
	err   string
}

type callbackServer struct {
	port int
	ch   chan callbackParams
}

func startCallbackServer() (string, *callbackServer, func(), error) {
	cb := &callbackServer{ch: make(chan callbackParams, 1)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, nil, fmt.Errorf("listen for oauth callback: %w", err)
	}
	cb.port = ln.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p := callbackParams{
			code:  q.Get("code"),
			state: q.Get("state"),
			err:   q.Get("error"),
		}
		select {
		case cb.ch <- p:
		default:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body style="font-family:sans-serif;text-align:center;padding-top:80px">
<p>✅ Login callback received. You can close this tab and go back to the terminal.</p></body></html>`)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	state := randomState()
	closeFn := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
	return state, cb, closeFn, nil
}

func randomState() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

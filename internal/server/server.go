// Package server exposes the 9router-compatible search API plus admin endpoints.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"ggsearch/internal/manager"
	"ggsearch/internal/store"
)

// Deps wires the server together.
type Deps struct {
	Store        *store.Store
	Manager      *manager.Manager
	Log          *slog.Logger
	AdminToken   string // when empty, admin endpoints are disabled
	DefaultModel string
}

// Handler builds the HTTP mux.
func Handler(d Deps) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	mux.HandleFunc("POST /v1/search", d.handleSearch)

	// Admin endpoints: protected by the static admin token.
	mux.HandleFunc("GET /v1/keys", d.admin(d.handleListKeys))
	mux.HandleFunc("POST /v1/keys", d.admin(d.handleCreateKey))
	mux.HandleFunc("DELETE /v1/keys/{id}", d.admin(d.handleDeleteKey))
	mux.HandleFunc("GET /v1/accounts", d.admin(d.handleListAccounts))
	mux.HandleFunc("DELETE /v1/accounts/{id}", d.admin(d.handleDeleteAccount))
	mux.HandleFunc("POST /v1/accounts/{id}/disable", d.admin(d.handleToggleAccount(false)))
	mux.HandleFunc("POST /v1/accounts/{id}/enable", d.admin(d.handleToggleAccount(true)))
	mux.HandleFunc("GET /v1/usage", d.admin(d.handleUsage))

	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errorJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// authAdmin checks the admin token.
func (d Deps) authAdmin(r *http.Request) bool {
	if d.AdminToken == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("Authorization"))
	return subtleEqual("Bearer "+d.AdminToken, got)
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func (d Deps) admin(next func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.authAdmin(r) {
			errorJSON(w, http.StatusUnauthorized, "invalid admin token")
			return
		}
		next(w, r)
	}
}

// --- search endpoint (9router /v1/search compatible) ---

type searchRequest struct {
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

// Result shapes mirror 9router's unified /v1/search response.

type searchResultItem struct {
	Title       string         `json:"title"`
	URL         string         `json:"url"`
	Snippet     string         `json:"snippet"`
	Position    int            `json:"position"`
	Score       any            `json:"score"`
	PublishedAt any            `json:"published_at"`
	FaviconURL  any            `json:"favicon_url"`
	Content     *string        `json:"content"`
	Metadata    map[string]any `json:"metadata"`
	Citation    resultCitation `json:"citation"`
}

type resultCitation struct {
	Provider    string `json:"provider"`
	RetrievedAt string `json:"retrieved_at"`
	Rank        int    `json:"rank"`
}

type searchResponse struct {
	Provider string             `json:"provider"`
	Query    string             `json:"query"`
	Results  []searchResultItem `json:"results"`
	Answer   *answerPayload     `json:"answer"`
	Usage    usagePayload       `json:"usage"`
	Metrics  metricsPayload     `json:"metrics"`
	Errors   []any              `json:"errors"`
}

type answerPayload struct {
	Source string `json:"source"`
	Text   string `json:"text"`
	Model  string `json:"model"`
}

type usagePayload struct {
	QueriesUsed   int     `json:"queries_used"`
	SearchCostUSD float64 `json:"search_cost_usd"`
	LLMTokens     int64   `json:"llm_tokens"`
}

type metricsPayload struct {
	ResponseTimeMS        int64 `json:"response_time_ms"`
	UpstreamLatencyMS     int64 `json:"upstream_latency_ms"`
	TotalResultsAvailable any   `json:"total_results_available"`
}

func (d Deps) handleSearch(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	key, ok := d.authenticateKey(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ggsearch"`)
		errorJSON(w, http.StatusUnauthorized, "missing or invalid API key")
		return
	}

	var req searchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		errorJSON(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		errorJSON(w, http.StatusBadRequest, "Missing query")
		return
	}
	// 9router treats model/provider as interchangeable aliases for the provider id.
	if req.Provider != "" && req.Model == "" {
		req.Model = req.Provider
	}
	model := req.Model
	if model == "" || model == "antigravity" || model == "ag" {
		model = d.DefaultModel
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	res, acc, err := d.Manager.Search(ctx, req.Query, model)
	if err != nil {
		if errors.Is(err, manager.ErrNoAccounts) {
			errorJSON(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		var apiErr *apiError
		if errors.As(err, &apiErr) {
			errorJSON(w, apiErr.Status, apiErr.Message)
			return
		}
		errorJSON(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}

	_ = d.Store.TouchAPIKey(ctx, key.ID)
	go func() {
		uCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.Store.RecordUsage(uCtx, acc.ID, key.ID, truncateStr(req.Query, 500), model, time.Since(start).Milliseconds())
	}()

	limit := req.MaxResults
	if limit <= 0 {
		limit = 10
	}
	retrievedAt := time.Now().UTC().Format(time.RFC3339)

	items := make([]searchResultItem, 0, len(res.Citations))
	for i, c := range res.Citations {
		if i >= limit {
			break
		}
		content := c.Content
		items = append(items, searchResultItem{
			Title:       c.Title,
			URL:         c.URL,
			Snippet:     c.Snippet,
			Position:    i + 1,
			Score:       nil,
			PublishedAt: nil,
			FaviconURL:  nil,
			Content:     &content,
			Metadata:    map[string]any{},
			Citation: resultCitation{
				Provider:    "antigravity",
				RetrievedAt: retrievedAt,
				Rank:        i + 1,
			},
		})
	}

	writeJSON(w, http.StatusOK, searchResponse{
		Provider: "antigravity",
		Query:    req.Query,
		Results:  items,
		Answer: &answerPayload{
			Source: "antigravity",
			Text:   res.Answer,
			Model:  res.Model,
		},
		Usage: usagePayload{
			QueriesUsed:   1,
			SearchCostUSD: 0,
			LLMTokens:     res.Tokens,
		},
		Metrics: metricsPayload{
			ResponseTimeMS:        time.Since(start).Milliseconds(),
			UpstreamLatencyMS:     time.Since(start).Milliseconds(),
			TotalResultsAvailable: len(res.Citations),
		},
		Errors: []any{},
	})
}

// apiError lets upstream status codes pass through.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return e.Message }

// authenticateKey resolves the bearer key against the store.
func (d Deps) authenticateKey(r *http.Request) (*store.APIKey, bool) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(auth, "Bearer ") {
		return nil, false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if raw == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])
	key, err := d.Store.FindAPIKeyByHash(r.Context(), hash)
	if err != nil || !key.IsActive {
		return nil, false
	}
	return key, true
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// --- admin: keys ---

func (d Deps) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := d.Store.ListAPIKeys(r.Context())
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	usage24h, _ := d.Store.UsageSummary(r.Context(), 24*time.Hour)
	usage30d, _ := d.Store.UsageSummary(r.Context(), 30*24*time.Hour)
	type keyOut struct {
		ID         string  `json:"id"`
		Label      string  `json:"label"`
		Prefix     string  `json:"prefix"`
		IsActive   bool    `json:"is_active"`
		CreatedAt  string  `json:"created_at"`
		LastUsedAt *string `json:"last_used_at"`
		Usage24h   int64   `json:"usage_24h"`
		Usage30d   int64   `json:"usage_30d"`
	}
	out := make([]keyOut, 0, len(keys))
	for _, k := range keys {
		var lastUsed *string
		if k.LastUsedAt != nil {
			s := k.LastUsedAt.Format(time.RFC3339)
			lastUsed = &s
		}
		out = append(out, keyOut{
			ID: k.ID, Label: k.Label, Prefix: k.Prefix, IsActive: k.IsActive,
			CreatedAt: k.CreatedAt.Format(time.RFC3339), LastUsedAt: lastUsed,
			Usage24h: usage24h[k.ID], Usage30d: usage30d[k.ID],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (d Deps) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		// allow empty body
		body.Label = ""
	}
	full, _, _ := store.NewAPIKeyMaterial()
	k, err := d.Store.CreateAPIKey(r.Context(), body.Label, full)
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": k.ID, "label": k.Label, "prefix": k.Prefix, "key": full,
		"note": "store this key now — it is not retrievable later",
	})
}

func (d Deps) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := d.Store.DeleteAPIKey(r.Context(), id); err != nil {
		errorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// --- admin: accounts ---

func (d Deps) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := d.Manager.Accounts(r.Context())
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	type accOut struct {
		ID           string  `json:"id"`
		Email        string  `json:"email"`
		ProjectID    string  `json:"project_id"`
		IsActive     bool    `json:"is_active"`
		FailureCount int     `json:"failure_count"`
		LastError    string  `json:"last_error"`
		LastSearchAt *string `json:"last_search_at"`
	}
	out := make([]accOut, 0, len(accounts))
	for _, a := range accounts {
		var ls *string
		if a.LastSearchAt != nil {
			s := a.LastSearchAt.Format(time.RFC3339)
			ls = &s
		}
		errMsg := a.LastError
		if len(errMsg) > 300 {
			errMsg = errMsg[:300]
		}
		out = append(out, accOut{
			ID: a.ID, Email: a.Email, ProjectID: a.ProjectID, IsActive: a.IsActive,
			FailureCount: a.FailureCount, LastError: errMsg, LastSearchAt: ls,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func (d Deps) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := d.Store.DeleteAccount(r.Context(), id); err != nil {
		errorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (d Deps) handleToggleAccount(active bool) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := d.Store.SetAccountActive(r.Context(), id, active); err != nil {
			errorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "is_active": active})
	}
}

// --- admin: usage ---

func (d Deps) handleUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lookback := 24 * time.Hour
	if v := q.Get("window"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			lookback = d
		}
	}
	summary, err := d.Store.UsageSummary(r.Context(), lookback)
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	keys, err := d.Store.ListAPIKeys(r.Context())
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows := make([]map[string]any, 0, len(keys))
	var total int64
	for _, k := range keys {
		n := summary[k.ID]
		total += n
		rows = append(rows, map[string]any{"key_id": k.ID, "label": k.Label, "prefix": k.Prefix, "count": n})
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": lookback.String(), "total": total, "by_key": rows})
}

var _ = fmt.Sprintf // keep fmt imported for future handlers

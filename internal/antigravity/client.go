// Package antigravity talks to Google's Antigravity (CloudCode) backend the same
// way the 9router project does: OAuth2 with the public Antigravity IDE client,
// then generateContent with Google Search grounding.
package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Public Antigravity IDE client, as shipped in 9router (open-sse/providers/shared.js).
const (
	ideBaseURL   = "https://daily-cloudcode-pa.googleapis.com"
	cloudBase    = "https://cloudcode-pa.googleapis.com"
	tokenURL     = "https://oauth2.googleapis.com/token"
	authorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	userInfoURL  = "https://www.googleapis.com/oauth2/v1/userinfo"

	ideUserAgent = "antigravity/ide/2.11.0 darwin/arm64"

	DefaultModel = "gemini-2.5-flash"
)

// Public Antigravity IDE OAuth client — the same constants 9router ships in
// its open-source repo (open-sse/providers/shared.js). Not present as
// literals in this repo (GitHub push protection blocks them); injected at
// build time via -ldflags. See scripts/build.ps1 / scripts/build.sh.
var (
	clientID     string
	clientSecret string
)

var scopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

// requireOAuthClient fails fast when the build did not inject the OAuth client.
func requireOAuthClient() error {
	if clientID == "" || clientSecret == "" {
		return fmt.Errorf("OAuth client not configured — build with scripts/build.ps1 (or scripts/build.sh), or inject via -ldflags: " +
			`-X ggsearch/internal/antigravity.clientID=<id> -X ggsearch/internal/antigravity.clientSecret=<secret>`)
	}
	return nil
}

// Client is a low-level Antigravity API client.
type Client struct {
	HTTP *http.Client
}

// New creates a client with sensible defaults.
func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) doPost(ctx context.Context, rawURL string, headers map[string]string, body any, out any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		msg := string(data)
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return &APIError{Status: resp.StatusCode, Body: msg}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// APIError is a non-2xx upstream response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("antigravity: HTTP %d: %s", e.Status, e.Body)
}

// --- OAuth ---

// BuildAuthorizeURL builds the Google OAuth consent URL for the local callback.
func BuildAuthorizeURL(redirectURI, state string) (string, error) {
	if err := requireOAuthClient(); err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", state)
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	return authorizeURL + "?" + q.Encode(), nil
}

// TokenResponse is the OAuth token payload from Google.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// ExchangeCode trades an authorization code for tokens.
func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI string) (*TokenResponse, error) {
	if err := requireOAuthClient(); err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("token exchange failed: HTTP %d: %s", resp.StatusCode, truncate(string(data), 500))
	}
	var tr TokenResponse
	if err := json.Unmarshal(data, &tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	return &tr, nil
}

// RefreshToken exchanges a refresh token for a new access token.
func (c *Client) RefreshToken(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	if err := requireOAuthClient(); err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("refresh_token", refreshToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("token refresh failed: HTTP %d: %s", resp.StatusCode, truncate(string(data), 500))
	}
	var tr TokenResponse
	if err := json.Unmarshal(data, &tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	return &tr, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// UserInfo is the subset of the Google userinfo payload we need.
type UserInfo struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// GetUserEmail fetches the account email for an access token.
func (c *Client) GetUserEmail(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userInfoURL+"?alt=json", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("userinfo: HTTP %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var ui UserInfo
	if err := json.Unmarshal(data, &ui); err != nil {
		return "", err
	}
	if ui.Email == "" {
		return "", fmt.Errorf("userinfo: no email in response")
	}
	return ui.Email, nil
}

// --- loadCodeAssist / onboarding ---

type clientMetadata struct {
	IdeType    int `json:"ideType"`
	Platform   int `json:"platform"`
	PluginType int `json:"pluginType"`
}

// windows=5 in the Antigravity binary enum (darwin arm64=2, x64=1, linux x64=3, arm64=4, win=5)
var runtimeMetadata = clientMetadata{IdeType: 9, Platform: 5, PluginType: 2}

type loadCodeAssistResp struct {
	CloudAICompanionProject any `json:"cloudaicompanionProject"`
	AllowedTiers            []struct {
		ID        string `json:"id"`
		IsDefault bool   `json:"isDefault"`
	} `json:"allowedTiers"`
}

func extractProjectID(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t), t != ""
	case map[string]any:
		id, _ := t["id"].(string)
		return strings.TrimSpace(id), id != ""
	}
	return "", false
}

// LoadProjectID calls v1internal:loadCodeAssist to fetch the user's cloudaicompanionProject.
func (c *Client) LoadProjectID(ctx context.Context, accessToken string) (projectID string, tierID string, err error) {
	var resp loadCodeAssistResp
	headers := map[string]string{
		"Authorization": "Bearer " + accessToken,
		"User-Agent":    ideUserAgent,
	}
	body := map[string]any{"metadata": runtimeMetadata}
	if err := c.doPost(ctx, cloudBase+"/v1internal:loadCodeAssist", headers, body, &resp); err != nil {
		return "", "", err
	}
	pid, ok := extractProjectID(resp.CloudAICompanionProject)
	if !ok {
		return "", "", fmt.Errorf("loadCodeAssist: no cloudaicompanionProject in response")
	}
	tier := "legacy-tier"
	for _, t := range resp.AllowedTiers {
		if t.IsDefault && t.ID != "" {
			tier = strings.TrimSpace(t.ID)
			break
		}
	}
	return pid, tier, nil
}

// --- search (grounded generateContent) ---

type searchRequestEnvelope struct {
	Project     string       `json:"project"`
	Model       string       `json:"model"`
	UserAgent   string       `json:"userAgent"`
	RequestType string       `json:"requestType"`
	Request     generateBody `json:"request"`
}

type generateBody struct {
	Contents         []content     `json:"contents"`
	Tools            []tool        `json:"tools"`
	GenerationConfig generationCfg `json:"generationConfig"`
}

type content struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

type part struct {
	Text string `json:"text"`
}

type tool struct {
	// no omitempty: an empty map must serialize as "googleSearch":{}, otherwise
	// the upstream drops the grounding tool entirely.
	GoogleSearch map[string]any `json:"googleSearch"`
}

type generationCfg struct {
	Temperature     float64 `json:"temperature"`
	MaxOutputTokens int     `json:"maxOutputTokens"`
}

// SearchArgs controls one grounded search call.
type SearchArgs struct {
	Query string
	Model string // defaults to DefaultModel
}

// SearchResult is the normalized result of one search.
type SearchResult struct {
	Answer    string
	Model     string
	Tokens    int64
	Citations []Citation
}

// Citation is one deduped web source with its grounded snippets.
type Citation struct {
	URL     string
	Title   string
	Snippet string
	Content string
}

// upstream response shapes

type genResponse struct {
	Response *candidateWrapper `json:"response"`
}

type candidateWrapper struct {
	Candidates []candidate `json:"candidates"`
	UsageMeta  struct {
		TotalTokenCount int64 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

type candidate struct {
	Content struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"content"`
	GroundingMetadata struct {
		GroundingChunks []struct {
			Web struct {
				URI   string `json:"uri"`
				URL   string `json:"url"`
				Title string `json:"title"`
			} `json:"web"`
		} `json:"groundingChunks"`
		GroundingSupports []struct {
			Segment struct {
				Text       string `json:"text"`
				StartIndex int    `json:"startIndex"`
				EndIndex   int    `json:"endIndex"`
			} `json:"segment"`
			Indices []int `json:"groundingChunkIndices"`
		} `json:"groundingSupports"`
	} `json:"groundingMetadata"`
}

// Search runs one grounded search against the Antigravity backend.
func (c *Client) Search(ctx context.Context, accessToken, projectID string, args SearchArgs) (*SearchResult, error) {
	model := args.Model
	if model == "" {
		model = DefaultModel
	}
	env := searchRequestEnvelope{
		Project:     projectID,
		Model:       model,
		UserAgent:   "antigravity",
		RequestType: "search",
		Request: generateBody{
			Contents:         []content{{Role: "user", Parts: []part{{Text: args.Query}}}},
			Tools:            []tool{{GoogleSearch: map[string]any{}}},
			GenerationConfig: generationCfg{Temperature: 1.0, MaxOutputTokens: 8192},
		},
	}
	var raw genResponse
	headers := map[string]string{
		"Authorization": "Bearer " + accessToken,
		"User-Agent":    ideUserAgent,
	}
	if err := c.doPost(ctx, ideBaseURL+"/v1internal:generateContent", headers, env, &raw); err != nil {
		return nil, err
	}

	resp := raw.Response
	if resp == nil {
		resp = &candidateWrapper{}
	}
	var sb strings.Builder
	if len(resp.Candidates) > 0 {
		for _, p := range resp.Candidates[0].Content.Parts {
			sb.WriteString(p.Text)
		}
	}
	result := &SearchResult{
		Answer: sb.String(),
		Model:  model,
		Tokens: resp.UsageMeta.TotalTokenCount,
	}

	// Dedupe citations by URL, collecting grounded snippets per source.
	type src struct {
		title    string
		snippets map[string]struct{}
		contexts map[string]struct{}
	}
	text := result.Answer
	sources := map[string]*src{}
	var order []string
	byIndex := make([]*src, len(resp.Candidates[0].GroundingMetadata.GroundingChunks))
	cand := resp.Candidates[0]
	for i, ch := range cand.GroundingMetadata.GroundingChunks {
		u := ch.Web.URI
		if u == "" {
			u = ch.Web.URL
		}
		if u == "" {
			continue
		}
		s, ok := sources[u]
		if !ok {
			s = &src{title: ch.Web.Title, snippets: map[string]struct{}{}, contexts: map[string]struct{}{}}
			sources[u] = s
			order = append(order, u)
		}
		if i < len(byIndex) {
			byIndex[i] = s
		}
	}
	for _, sup := range cand.GroundingMetadata.GroundingSupports {
		segmentText := sup.Segment.Text
		expanded := expandSegment(text, sup.Segment.StartIndex, sup.Segment.EndIndex)
		if expanded == "" {
			expanded = segmentText
		}
		for _, idx := range sup.Indices {
			if idx < 0 || idx >= len(byIndex) || byIndex[idx] == nil {
				continue
			}
			s := byIndex[idx]
			if segmentText != "" {
				s.snippets[segmentText] = struct{}{}
			}
			if expanded != "" {
				s.contexts[expanded] = struct{}{}
			}
		}
	}
	for _, u := range order {
		s := sources[u]
		snippet := joinSet(s.snippets, " | ")
		if snippet == "" {
			snippet = s.title
		}
		contentStr := joinSet(s.contexts, "\n\n")
		if contentStr == "" {
			contentStr = snippet
		}
		result.Citations = append(result.Citations, Citation{
			URL: u, Title: s.title, Snippet: snippet, Content: contentStr,
		})
	}
	return result, nil
}

const (
	contextBefore = 150
	contextAfter  = 250
)

// expandSegment widens a grounded segment to surrounding sentences in the answer.
func expandSegment(text string, startIndex, endIndex int) string {
	if text == "" {
		return ""
	}
	if startIndex < 0 || endIndex < 0 || startIndex > len(text) || endIndex > len(text) || startIndex > endIndex {
		return ""
	}
	start := max(0, startIndex-contextBefore)
	end := min(len(text), endIndex+contextAfter)
	out := strings.TrimSpace(text[start:end])
	if start > 0 {
		out = "..." + strings.TrimLeft(out, " \t\n")
		// drop the partial word at the left edge
		if i := strings.IndexAny(out[3:], " \t\n"); i >= 0 {
			out = "..." + out[3+i:]
		} else {
			out = "..."
		}
	}
	if end < len(text) {
		// drop the partial word at the right edge
		if i := strings.LastIndexAny(out, " \t\n"); i >= 0 {
			out = out[:i] + "..."
		} else {
			out = out + "..."
		}
	}
	return strings.TrimSpace(out)
}

func joinSet(set map[string]struct{}, sep string) string {
	var parts []string
	for k := range set {
		if k != "" {
			parts = append(parts, k)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	sortStrings(parts)
	return strings.Join(parts, sep)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

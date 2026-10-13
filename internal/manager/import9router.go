package manager

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// 9router stores provider connections as a JSON blob in its SQLite db.
type routerRow struct {
	email        string
	accessToken  string
	refreshToken string
	projectID    string
	expiresAt    string
}

func read9RouterAntigravityRows(dbPath string) ([]routerRow, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=mode(ro)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT email, data FROM providerConnections WHERE provider = 'antigravity'`)
	if err != nil {
		return nil, fmt.Errorf("query 9router db: %w", err)
	}
	defer rows.Close()
	var out []routerRow
	for rows.Next() {
		var email, data string
		if err := rows.Scan(&email, &data); err != nil {
			return nil, err
		}
		var blob struct {
			Email        string `json:"email"`
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ProjectID    string `json:"projectId"`
			ExpiresAt    string `json:"expiresAt"`
		}
		if err := json.Unmarshal([]byte(data), &blob); err != nil {
			continue // skip malformed rows
		}
		if strings.TrimSpace(blob.RefreshToken) == "" {
			continue
		}
		e := strings.TrimSpace(email)
		if e == "" {
			e = strings.TrimSpace(blob.Email) // fallback if column empty
		}
		out = append(out, routerRow{
			email:        e,
			accessToken:  blob.AccessToken,
			refreshToken: blob.RefreshToken,
			projectID:    blob.ProjectID,
			expiresAt:    blob.ExpiresAt,
		})
	}
	return out, rows.Err()
}

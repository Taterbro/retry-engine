package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Handlers holds shared dependencies for all HTTP routes.
type Handlers struct {
	db *sql.DB
}

func NewHandlers(db *sql.DB) *Handlers { return &Handlers{db: db} }

// ── POST /request ─────────────────────────────────────────────────────────────

type createRequestInput struct {
	URL        string  `json:"url"`
	Method     string  `json:"method"`
	Body       *string `json:"body"`
	MaxRetries *int    `json:"maxRetries"`
	BackoffMs  *int    `json:"backoffMs"`
}

func (h *Handlers) CreateRequest(w http.ResponseWriter, r *http.Request) {
	var in createRequestInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if in.URL == "" || in.Method == "" {
		jsonError(w, "url and method are required", http.StatusBadRequest)
		return
	}

	maxRetries := 5
	if in.MaxRetries != nil {
		maxRetries = *in.MaxRetries
	}
	backoffMs := 1000
	if in.BackoffMs != nil {
		backoffMs = *in.BackoffMs
	}

	id := newID()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// next_retry_at is set to now so the worker picks it up on the very
	// next tick (within ~500 ms).
	_, err := h.db.Exec(`
		INSERT INTO requests
			(id, url, method, body, status, attempt_count, max_retries, backoff_ms, next_retry_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'pending', 0, ?, ?, ?, ?, ?)
	`, id, in.URL, in.Method, in.Body, maxRetries, backoffMs, now, now, now)
	if err != nil {
		jsonError(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted) // 202 — request accepted, processing async
	json.NewEncoder(w).Encode(map[string]any{"id": id, "status": "pending"})
}

// ── GET /requests/{id} ────────────────────────────────────────────────────────

func (h *Handlers) GetRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	req, err := fetchRequest(h.db, id)
	if err == sql.ErrNoRows {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	req.Attempts, err = fetchAttempts(h.db, id)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, req)
}

// ── GET /requests?status=failed ───────────────────────────────────────────────

func (h *Handlers) ListRequests(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")

	var (
		rows *sql.Rows
		err  error
	)
	const cols = `id,url,method,body,status,attempt_count,max_retries,backoff_ms,next_retry_at,last_error,result,created_at,updated_at`
	if status != "" {
		rows, err = h.db.Query(`SELECT `+cols+` FROM requests WHERE status=? ORDER BY created_at DESC`, status)
	} else {
		rows, err = h.db.Query(`SELECT ` + cols + ` FROM requests ORDER BY created_at DESC`)
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var reqs []Request
	for rows.Next() {
		req, err := scanRequest(rows)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		reqs = append(reqs, *req)
	}
	if reqs == nil {
		reqs = []Request{} // return [] not null
	}
	jsonOK(w, reqs)
}

// ── DB helpers ────────────────────────────────────────────────────────────────

// scanner is satisfied by both *sql.Row and *sql.Rows, letting us reuse scanRequest.
type scanner interface{ Scan(...any) error }

func scanRequest(s scanner) (*Request, error) {
	var req Request
	var body, nextRetryAt, lastError, result sql.NullString
	var createdAt, updatedAt string

	err := s.Scan(
		&req.ID, &req.URL, &req.Method, &body,
		&req.Status, &req.AttemptCount, &req.MaxRetries, &req.BackoffMs,
		&nextRetryAt, &lastError, &result,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	if body.Valid {
		req.Body = &body.String
	}
	if lastError.Valid {
		req.LastError = &lastError.String
	}
	if result.Valid {
		req.Result = &result.String
	}
	req.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	req.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	if nextRetryAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, nextRetryAt.String)
		req.NextRetryAt = &t
	}
	return &req, nil
}

func fetchRequest(db *sql.DB, id string) (*Request, error) {
	row := db.QueryRow(`
		SELECT id,url,method,body,status,attempt_count,max_retries,backoff_ms,next_retry_at,last_error,result,created_at,updated_at
		FROM requests WHERE id=?`, id)
	return scanRequest(row)
}

func fetchAttempts(db *sql.DB, requestID string) ([]Attempt, error) {
	rows, err := db.Query(`
		SELECT id,request_id,attempt_num,status_code,error,response,attempted_at
		FROM attempts WHERE request_id=? ORDER BY attempt_num ASC`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Attempt
	for rows.Next() {
		var a Attempt
		var code sql.NullInt64
		var errStr, response sql.NullString
		var at string
		if err := rows.Scan(&a.ID, &a.RequestID, &a.AttemptNum, &code, &errStr, &response, &at); err != nil {
			return nil, err
		}
		if code.Valid {
			c := int(code.Int64)
			a.StatusCode = &c
		}
		if errStr.Valid {
			a.Error = &errStr.String
		}
		if response.Valid {
			a.Response = &response.String
		}
		a.AttemptedAt, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, a)
	}
	if out == nil {
		out = []Attempt{}
	}
	return out, nil
}

// ── Utilities ─────────────────────────────────────────────────────────────────

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

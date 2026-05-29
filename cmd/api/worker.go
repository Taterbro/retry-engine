package main

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// Worker is the background goroutine that executes HTTP calls and manages
// retries. Only one instance should run.
type Worker struct {
	db     *sql.DB
	client *http.Client
}

func NewWorker(db *sql.DB) *Worker {
	return &Worker{
		db:     db,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Run ticks every 500 ms and processes any requests whose next_retry_at has
// passed. Blocks forever; call as a goroutine.
func (w *Worker) Run() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		if err := w.processReady(); err != nil {
			log.Printf("worker: processReady error: %v", err)
		}
	}
}

// pendingRow is a lightweight copy of what we need from the DB.
type pendingRow struct {
	id, url, method string
	body            sql.NullString
	attemptCount    int
	maxRetries      int
	backoffMs       int
}

// processReady finds all rows that are due and attempts each one.
func (w *Worker) processReady() error {
	now := time.Now().UTC().Format(time.RFC3339Nano)

	rows, err := w.db.Query(`
		SELECT id, url, method, body, attempt_count, max_retries, backoff_ms
		FROM requests
		WHERE status IN ('pending', 'retrying')
		  AND next_retry_at <= ?
	`, now)
	if err != nil {
		return err
	}

	// Collect first so we close the result-set before issuing UPDATEs.
	// (SQLite doesn't support concurrent read+write on the same connection.)
	var ready []pendingRow
	for rows.Next() {
		var r pendingRow
		if err := rows.Scan(&r.id, &r.url, &r.method, &r.body,
			&r.attemptCount, &r.maxRetries, &r.backoffMs); err != nil {
			rows.Close()
			return err
		}
		ready = append(ready, r)
	}
	rows.Close()

	for _, r := range ready {
		w.attempt(r)
	}
	return nil
}

// attempt performs one HTTP call and updates the DB accordingly.
func (w *Worker) attempt(r pendingRow) {
	attemptNum := r.attemptCount + 1

	// ── 1. Make the HTTP request ─────────────────────────────────────────────

	var bodyReader io.Reader
	if r.body.Valid && r.body.String != "" {
		bodyReader = strings.NewReader(r.body.String)
	}

	var (
		statusCode  *int
		errStr      *string
		result      *string
		shouldRetry bool
		is4xx       bool
	)

	req, err := http.NewRequest(r.method, r.url, bodyReader)
	if err != nil {
		s := fmt.Sprintf("build request: %v", err)
		errStr = &s
		shouldRetry = true
	} else {
		resp, err := w.client.Do(req)
		if err != nil {
			// Network error or timeout → retry.
			s := err.Error()
			errStr = &s
			shouldRetry = true
		} else {
			defer resp.Body.Close()
			code := resp.StatusCode
			statusCode = &code
			b, _ := io.ReadAll(resp.Body)
			s := string(b)
			result = &s

			switch {
			case resp.StatusCode >= 500:
				// 5xx → retry.
				s2 := fmt.Sprintf("server error %d", resp.StatusCode)
				errStr = &s2
				shouldRetry = true
			case resp.StatusCode >= 400:
				// 4xx → permanent failure, never retry.
				s2 := fmt.Sprintf("client error %d (not retryable)", resp.StatusCode)
				errStr = &s2
				is4xx = true
				// default: 2xx / 3xx → success, fall through
			}
		}
	}

	// ── 2. Persist the attempt record ────────────────────────────────────────

	attemptedAt := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = w.db.Exec(`
		INSERT INTO attempts (request_id, attempt_num, status_code, error, response, attempted_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, r.id, attemptNum, statusCode, errStr, result, attemptedAt)

	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)

	// ── 3. Update the request row ─────────────────────────────────────────────

	switch {
	case !shouldRetry && !is4xx:
		// ✅ Success
		_, _ = w.db.Exec(`
			UPDATE requests
			SET status='completed', attempt_count=?, result=?, last_error=NULL, updated_at=?
			WHERE id=?
		`, attemptNum, result, updatedAt, r.id)
		log.Printf("[%s] completed after %d attempt(s)", r.id, attemptNum)

	case is4xx:
		// 🚫 Client error — dead-letter immediately, do not retry
		_, _ = w.db.Exec(`
			UPDATE requests
			SET status='failed', attempt_count=?, last_error=?, updated_at=?
			WHERE id=?
		`, attemptNum, errStr, updatedAt, r.id)
		log.Printf("[%s] permanently failed (4xx) after %d attempt(s)", r.id, attemptNum)

	case attemptNum >= r.maxRetries:
		// 🪦 Exhausted all retries — dead-letter
		_, _ = w.db.Exec(`
			UPDATE requests
			SET status='failed', attempt_count=?, last_error=?, updated_at=?
			WHERE id=?
		`, attemptNum, errStr, updatedAt, r.id)
		log.Printf("[%s] dead-lettered after %d/%d attempts: %s", r.id, attemptNum, r.maxRetries, *errStr)

	default:
		// 🔄 Schedule next retry with exponential backoff + jitter
		//
		//   wait = backoffMs × 2^(attemptNum−1)
		//   e.g. attempt 1 → 1 s, attempt 2 → 2 s, attempt 3 → 4 s …
		//
		//   jitter multiplier is uniform in [0.8, 1.2) to desynchronise
		//   concurrent clients hitting the same upstream.
		wait := float64(r.backoffMs) * math.Pow(2, float64(attemptNum-1))
		jitter := 0.8 + rand.Float64()*0.4
		delay := time.Duration(wait*jitter) * time.Millisecond
		nextRetry := time.Now().UTC().Add(delay).Format(time.RFC3339Nano)

		_, _ = w.db.Exec(`
			UPDATE requests
			SET status='retrying', attempt_count=?, last_error=?, next_retry_at=?, updated_at=?
			WHERE id=?
		`, attemptNum, errStr, nextRetry, updatedAt, r.id)
		log.Printf("[%s] retrying (%d/%d) in %v", r.id, attemptNum+1, r.maxRetries, delay.Round(time.Millisecond))
	}
}

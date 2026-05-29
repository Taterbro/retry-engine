package main

import "time"

// Request is the top-level retry job saved in the DB.
type Request struct {
	ID           string     `json:"id"`
	URL          string     `json:"url"`
	Method       string     `json:"method"`
	Body         *string    `json:"body,omitempty"`
	Status       string     `json:"status"` // pending | retrying | completed | failed
	AttemptCount int        `json:"attemptCount"`
	MaxRetries   int        `json:"maxRetries"`
	BackoffMs    int        `json:"backoffMs"`
	NextRetryAt  *time.Time `json:"nextRetryAt,omitempty"`
	LastError    *string    `json:"lastError,omitempty"`
	Result       *string    `json:"result,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	Attempts     []Attempt  `json:"attempts,omitempty"`
}

// Attempt records a single HTTP call that was made for a Request.
type Attempt struct {
	ID          int       `json:"id"`
	RequestID   string    `json:"requestId"`
	AttemptNum  int       `json:"attemptNum"`
	StatusCode  *int      `json:"statusCode,omitempty"`
	Error       *string   `json:"error,omitempty"`
	Response    *string   `json:"response,omitempty"`
	AttemptedAt time.Time `json:"attemptedAt"`
}

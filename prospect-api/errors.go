package main

import (
	"encoding/json"
	"net/http"
)

// ErrorResponse is the standard error format across all endpoints, per PRD lock.
//
//	{ "error": { "code": string, "message": string, "retryable": bool } }
//
// HTTP status follows the REST convention; the body shape is uniform regardless.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// Error codes (kebab-case).
const (
	ErrInvalidRequest   = "invalid-request"
	ErrUnauthorized     = "unauthorized"
	ErrForbidden        = "forbidden"
	ErrNotFound         = "not-found"
	ErrConflict         = "conflict"
	ErrRateLimited      = "rate-limited"
	ErrInternal         = "internal-error"
	ErrServiceUnavailab = "service-unavailable"
	ErrIdentityMismatch = "identity-mismatch"
	ErrTokenExpired     = "token-expired"
)

func writeError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	writeJSON(w, status, ErrorResponse{Error: ErrorBody{
		Code: code, Message: message, Retryable: retryable,
	}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

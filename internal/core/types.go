// Package core owns Folio identities, authorization, and persisted actions.
// Both HTTP and MCP use this service; protocol adapters never supply identities
// from untrusted action parameters.
package core

import (
	"errors"
	"fmt"
)

type Actor struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Scopes    []string `json:"scopes,omitempty"`
	Resources []string `json:"resources,omitempty"`
}

func (a Actor) Public() Actor { return Actor{ID: a.ID, Name: a.Name, Kind: a.Kind} }

type Action struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	OutputSchema map[string]any `json:"output_schema,omitempty"`
	ReadOnly     bool           `json:"read_only"`
	Scope        string         `json:"scope"`
	Task         bool           `json:"task,omitempty"`
	Audit        bool           `json:"audited,omitempty"`
}

type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Error is the shared, safe protocol error. Internal database and OS errors are
// never put into its Message; adapters use these fields for REST and MCP.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
	Details any    `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// AgentError preserves the safe domain error across the authenticated Agent
// wire without exposing the underlying driver, filesystem or OS exception.
func (e *Error) AgentError() (string, int) { return e.Code, e.Status }
func ErrorStatus(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 500
}
func ErrorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "internal_error"
}
func invalid(message string) error {
	return &Error{Code: "invalid_input", Message: message, Status: 400}
}
func denied() error {
	return &Error{Code: "forbidden", Message: "This identity is not authorized for this action or resource", Status: 403}
}
func unauthorized() error {
	return &Error{Code: "unauthorized", Message: "Authentication required or credentials invalid", Status: 401}
}
func missing(name string) error {
	return &Error{Code: "not_found", Message: fmt.Sprintf("%s not found", name), Status: 404}
}
func internal() error {
	return &Error{Code: "internal_error", Message: "Operation could not be completed", Status: 500}
}

type Note struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Pinned    bool   `json:"pinned"`
	Archived  bool   `json:"archived"`
	TargetID  string `json:"target_id"`
	Revision  int64  `json:"revision"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	CreatedBy Actor  `json:"created_by"`
	UpdatedBy Actor  `json:"updated_by"`
}

type Audit struct {
	ID        string         `json:"id"`
	Action    string         `json:"action"`
	Actor     Actor          `json:"actor"`
	Resource  string         `json:"resource"`
	Status    string         `json:"status"`
	ErrorCode string         `json:"error_code,omitempty"`
	Summary   map[string]any `json:"summary,omitempty"`
	CreatedAt string         `json:"created_at"`
}

type TokenRequest struct {
	Name             string   `json:"name"`
	Kind             string   `json:"kind,omitempty"`
	Scopes           []string `json:"scopes"`
	Resources        []string `json:"resources"`
	ExpiresInSeconds int64    `json:"expires_in_seconds,omitempty"`
	Audience         string   `json:"audience,omitempty"`
}
type TokenInfo struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Scopes     []string `json:"scopes"`
	Resources  []string `json:"resources"`
	CreatedAt  string   `json:"created_at"`
	ExpiresAt  string   `json:"expires_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	Revoked    bool     `json:"revoked"`
	Audience   string   `json:"audience,omitempty"`
}
type TokenCreated struct {
	TokenInfo
	Token string `json:"token"`
}
type Session struct {
	ID        string `json:"-"`
	Actor     Actor  `json:"user"`
	CSRFToken string `json:"csrf_token"`
	ExpiresAt string `json:"expires_at"`
}

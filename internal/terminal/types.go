// Package terminal owns bounded PTY and SSH sessions. Its callers supply a
// trusted actor identity and authorize target/session resources before RPC.
// Local profiles are trusted operator configuration, never tool parameters.
package terminal

import (
	"context"
	"encoding/json"
	"time"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

func (e *Error) Error() string             { return e.Message }
func (e *Error) AgentError() (string, int) { return e.Code, e.Status }
func failure(code, message string, status int) *Error {
	return &Error{Code: code, Message: message, Status: status}
}

type MetadataStore interface {
	Save(context.Context, string, string, json.RawMessage) error
	Get(context.Context, string, string) (json.RawMessage, error)
	List(context.Context, string) (map[string]json.RawMessage, error)
	Delete(context.Context, string, string) error
}
type LocalProfile struct {
	ID      string `json:"id"`
	Label   string `json:"label,omitempty"`
	User    string `json:"user"`
	Shell   string `json:"shell"`
	Home    string `json:"home"`
	WorkDir string `json:"work_dir"`
}
type Config struct {
	APIUser        string         `json:"api_user"`
	StateDir       string         `json:"state_dir"`
	Profiles       []LocalProfile `json:"profiles"`
	EnableRoot     bool           `json:"enable_root,omitempty"`
	MaxSessions    int            `json:"max_sessions,omitempty"`
	MaxPerActor    int            `json:"max_per_actor,omitempty"`
	IdleSeconds    int            `json:"idle_seconds,omitempty"`
	LeaseSeconds   int            `json:"lease_seconds,omitempty"`
	ProtectedPaths []string       `json:"protected_paths,omitempty"`
}
type Credential struct {
	Password   []byte `json:"password,omitempty"`
	PrivateKey []byte `json:"private_key,omitempty"`
	Passphrase []byte `json:"passphrase,omitempty"`
}
type Options struct {
	Config        Config
	Store         MetadataStore
	Context       context.Context
	ResolveSecret func(context.Context, string, string, string) (Credential, error)
	// Go-only unit-test fixture. Not part of operator JSON and never enabled by
	// production constructors/configuration. VM tests verify separate OS UIDs.
	UnsafeSameUIDForTests bool
}
type Target struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Type      string `json:"type"`
	Host      string `json:"host,omitempty"`
	Port      int    `json:"port,omitempty"`
	Username  string `json:"username,omitempty"`
	AuthType  string `json:"auth_type,omitempty"`
	SecretRef string `json:"secret_ref,omitempty"`
	HostKey   string `json:"host_key,omitempty"`
	Root      bool   `json:"root"`
	CreatedBy string `json:"created_by,omitempty"`
	Revision  int64  `json:"revision"`
}
type Command struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Command   string `json:"command"`
	CreatedBy string `json:"created_by"`
	Revision  int64  `json:"revision"`
}
type OpenInput struct {
	TargetID         string `json:"target_id"`
	Rows             int    `json:"rows"`
	Cols             int    `json:"cols"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
	RootAuthorized   bool   `json:"-"`
}
type Input struct {
	ID               string `json:"id,omitempty"`
	TargetID         string `json:"target_id,omitempty"`
	Rows             int    `json:"rows,omitempty"`
	Cols             int    `json:"cols,omitempty"`
	After            uint64 `json:"after,omitempty"`
	Limit            int    `json:"limit,omitempty"`
	Data             string `json:"data,omitempty"`
	Label            string `json:"label,omitempty"`
	Host             string `json:"host,omitempty"`
	Port             int    `json:"port,omitempty"`
	Username         string `json:"username,omitempty"`
	AuthType         string `json:"auth_type,omitempty"`
	SecretRef        string `json:"secret_ref,omitempty"`
	HostKey          string `json:"host_key,omitempty"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
	Title            string `json:"title,omitempty"`
	Command          string `json:"command,omitempty"`
}
type Frame struct {
	Sequence uint64 `json:"sequence"`
	Data     string `json:"data"`
}
type SessionInfo struct {
	ID        string    `json:"id"`
	ActorID   string    `json:"actor_id"`
	TargetID  string    `json:"target_id"`
	Type      string    `json:"type"`
	Root      bool      `json:"root"`
	Rows      int       `json:"rows"`
	Cols      int       `json:"cols"`
	ProcessID int       `json:"process_id,omitempty"`
	Sequence  uint64    `json:"sequence"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Exited    bool      `json:"exited"`
	ExitCode  int       `json:"exit_code"`
	Reason    string    `json:"reason,omitempty"`
}
type ReadResult struct {
	SessionInfo
	Frames    []Frame `json:"frames"`
	Truncated bool    `json:"truncated"`
	Next      uint64  `json:"next"`
}

// AgentPayload exists only on the UID-authenticated panel-to-Agent connection.
// Public action JSON exposes Input alone; credentials/authorization flags must
// be resolved and added by the shared business adapter, never copied from tools.
type AgentPayload struct {
	Input            Input       `json:"input"`
	IssuedID         string      `json:"issued_id,omitempty"`
	Credential       *Credential `json:"credential,omitempty"`
	AuthorizedShared bool        `json:"authorized_shared,omitempty"`
	RootAuthorized   bool        `json:"root_authorized,omitempty"`
}
type Action struct {
	Name        string
	Scope       string
	ReadOnly    bool
	InputSchema map[string]any
}

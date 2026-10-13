// Package files implements the managed filesystem. Authentication and resource
// authorization belong to the shared business adapter; only trusted root IDs
// are accepted here. No client can install an absolute root or choose a program.
package files

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"time"
)

var ErrNotFound = fs.ErrNotExist

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
	Details any    `json:"details,omitempty"`
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

type RootSpec struct {
	internal        bool
	ID              string
	Label           string
	Path            string
	ReadOnly        bool
	HostAccess      bool // Explicit operator configuration; adapters require files:host in addition to files:read/write.
	ExpectedDevice  uint64
	ExpectedInode   uint64 // Optional root-owned receipt identity checked before registration/recovery.
	DefaultFileMode fs.FileMode
	DefaultDirMode  fs.FileMode
}
type Options struct {
	Roots                 []RootSpec
	StateDir              string
	ProtectedPaths        []string // Absolute private directories never exposed by managed roots.
	Store                 MetadataStore
	MaxTextBytes          int64
	MaxTransferBytes      int64
	MaxArchiveEntries     int
	DownloadHosts         []string
	AllowPrivateDownloads bool // Trusted development/test configuration only.
	AllowHTTPDownloads    bool
	DownloadClient        *http.Client      // Trusted transport override for isolated protocol tests.
	ArchiveTools          map[string]string // Trusted absolute programs: xz,bzip2,7z,rar.
}
type Ref struct {
	Root string `json:"root"`
	Path string `json:"path"`
}
type Input struct {
	Root            string            `json:"root,omitempty"`
	Path            string            `json:"path,omitempty"`
	DestinationRoot string            `json:"destination_root,omitempty"`
	DestinationPath string            `json:"destination_path,omitempty"`
	ExpectedHash    string            `json:"expected_hash,omitempty"`
	ExpectedHashes  map[string]string `json:"expected_hashes,omitempty"`
	Overwrite       bool              `json:"overwrite,omitempty"`
	Encoding        string            `json:"encoding,omitempty"`
	Content         string            `json:"content,omitempty"`
	Query           string            `json:"query,omitempty"`
	Sort            string            `json:"sort,omitempty"`
	Order           string            `json:"order,omitempty"`
	Offset          int               `json:"offset,omitempty"`
	Limit           int               `json:"limit,omitempty"`
	Recursive       bool              `json:"recursive,omitempty"`
	Mode            *uint32           `json:"mode,omitempty"`
	UID             *uint32           `json:"uid,omitempty"`
	GID             *uint32           `json:"gid,omitempty"`
	Items           []string          `json:"items,omitempty"`
	Format          string            `json:"format,omitempty"`
	ID              string            `json:"id,omitempty"`
	Size            int64             `json:"size,omitempty"`
	SHA256          string            `json:"sha256,omitempty"`
	URL             string            `json:"url,omitempty"`
	Lines           int               `json:"lines,omitempty"`
	Operation       string            `json:"operation,omitempty"`
	Enabled         *bool             `json:"enabled,omitempty"`
}
type Entry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	Mode       uint32    `json:"mode"`
	Owner      uint32    `json:"owner"`
	Group      uint32    `json:"group"`
	ModifiedAt time.Time `json:"modified_at"`
}
type Listing struct {
	Items  []Entry `json:"items"`
	Total  int     `json:"total"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
	Path   string  `json:"path"`
}
type Text struct {
	Content   string `json:"content"`
	Encoding  string `json:"encoding"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
}
type Revision struct {
	ID        string    `json:"id"`
	Root      string    `json:"root"`
	Path      string    `json:"path"`
	SHA256    string    `json:"sha256"`
	ActorID   string    `json:"actor_id"`
	CreatedAt time.Time `json:"created_at"`
	Size      int64     `json:"size"`
	Encoding  string    `json:"encoding"`
}
type Trash struct {
	ID        string    `json:"id"`
	Root      string    `json:"root"`
	Path      string    `json:"path"`
	ActorID   string    `json:"actor_id"`
	DeletedAt time.Time `json:"deleted_at"`
	Directory bool      `json:"directory"`
	State     string    `json:"state"`
}
type Upload struct {
	ID           string    `json:"id"`
	ActorID      string    `json:"actor_id"`
	Root         string    `json:"root"`
	RootDevice   uint64    `json:"root_device,omitempty"`
	RootInode    uint64    `json:"root_inode,omitempty"`
	Path         string    `json:"path"`
	Size         int64     `json:"size"`
	Offset       int64     `json:"offset"`
	ExpectedHash string    `json:"expected_hash"`
	SHA256       string    `json:"sha256"`
	ExpiresAt    time.Time `json:"expires_at"`
	State        string    `json:"state"`
	CopyMode     *uint32   `json:"copy_mode,omitempty"`
}
type Action struct {
	Name        string
	Scope       string
	ReadOnly    bool
	Task        bool
	InputSchema map[string]any
}

func IsCode(err error, code string) bool {
	var typed *Error
	return errors.As(err, &typed) && typed.Code == code
}

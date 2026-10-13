package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

// FileStore is useful for a standalone Agent or tests. The panel's shared
// SQLite store can implement Store instead. Records are atomically replaced,
// fsynced and stored under a private directory without modifying a checkout.
type FileStore struct{ files *agent.Filesystem }

var jobID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func NewFileStore(directory string) (*FileStore, error) {
	if !filepath.IsAbs(directory) {
		return nil, errors.New("job directory must be absolute")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("job directory must be private (0700)")
	}
	files, err := agent.NewFilesystem([]agent.RootSpec{{ID: "jobs", Path: directory}}, 128<<20)
	if err != nil {
		return nil, err
	}
	return &FileStore{files: files}, nil
}

func (s *FileStore) Close() error { return s.files.Close() }

func (s *FileStore) Save(ctx context.Context, job Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !jobID.MatchString(job.ID) {
		return errors.New("invalid job ID")
	}
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return s.files.WriteFile("jobs", job.ID+".json", data)
}

func (s *FileStore) Get(ctx context.Context, id string) (Job, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	if !jobID.MatchString(id) {
		return Job{}, ErrNotFound
	}
	data, err := s.files.ReadFile("jobs", id+".json")
	if errors.Is(err, fs.ErrNotExist) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return Job{}, err
	}
	if job.ID != id {
		return Job{}, errors.New("stored job ID mismatch")
	}
	return job, nil
}

func (s *FileStore) List(ctx context.Context) ([]Job, error) {
	entries, err := s.files.List("jobs", ".", 10000)
	if err != nil {
		return nil, err
	}
	all := make([]Job, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name, ".json") {
			continue
		}
		job, err := s.Get(ctx, strings.TrimSuffix(entry.Name, ".json"))
		if err != nil {
			return nil, err
		}
		all = append(all, job)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	return all, nil
}

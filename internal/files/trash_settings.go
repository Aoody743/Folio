package files

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"time"
)

type TrashSettings struct {
	Root      string    `json:"root"`
	Enabled   bool      `json:"enabled"`
	ActorID   string    `json:"actor_id,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

func (s *Service) TrashSettings(ctx context.Context, root string) (TrashSettings, error) {
	if _, _, err := s.resolve(Ref{root, "."}, false); err != nil {
		return TrashSettings{}, err
	}
	value, err := s.store.Get(ctx, "file_trash_settings", root)
	if errors.Is(err, fs.ErrNotExist) {
		return TrashSettings{Root: root, Enabled: true}, nil
	}
	if err != nil {
		return TrashSettings{}, err
	}
	var settings TrashSettings
	if err := json.Unmarshal(value, &settings); err != nil {
		return settings, err
	}
	if settings.Root != root {
		return settings, errors.New("invalid trash-settings root")
	}
	return settings, nil
}

func (s *Service) ConfigureTrash(ctx context.Context, actorID string, in Input) (TrashSettings, error) {
	if in.Enabled == nil || actorID == "" {
		return TrashSettings{}, failure("invalid_input", "Trash setting needs an explicit boolean and trusted actor", 400)
	}
	root, _, err := s.resolve(Ref{in.Root, "."}, false)
	if err != nil {
		return TrashSettings{}, err
	}
	if root.spec.ReadOnly {
		return TrashSettings{}, failure("read_only", "Root is read-only", 403)
	}
	unlock, err := s.lock(ctx, in.Root)
	if err != nil {
		return TrashSettings{}, err
	}
	defer unlock()
	settings := TrashSettings{Root: in.Root, Enabled: *in.Enabled, ActorID: actorID, UpdatedAt: time.Now().UTC()}
	if err := s.store.Save(ctx, "file_trash_settings", in.Root, encode(settings)); err != nil {
		return settings, err
	}
	return settings, nil
}

func (s *Service) ClearTrash(ctx context.Context, in Input) (BatchResult, error) {
	result := BatchResult{Items: []BatchOutcome{}}
	all, err := s.TrashList(ctx, in)
	if err != nil {
		return result, err
	}
	for _, record := range all.([]Trash) {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if record.State == "cleanup_required" {
			result.Failed++
			result.Items = append(result.Items, BatchOutcome{Name: record.ID, Code: "recovery_required"})
			continue
		}
		data, err := s.Restore(ctx, Input{Root: in.Root, ID: record.ID}, true)
		outcome := BatchOutcome{Name: record.ID, Completed: err == nil, Data: data}
		if err != nil {
			outcome.Code = "file_operation_failed"
			result.Failed++
		} else {
			result.Completed++
		}
		result.Items = append(result.Items, outcome)
	}
	if result.Failed > 0 {
		return result, &Error{Code: "partial_failure", Message: "Some recycle-bin entries could not be purged", Status: 409, Details: result}
	}
	return result, nil
}

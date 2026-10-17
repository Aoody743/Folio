package terminal

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type resolvedProfile struct {
	LocalProfile
	uid, gid uint32
	root     bool
}
type session struct {
	mu           sync.Mutex
	info         SessionInfo
	input        io.Writer
	resize       func(int, int) error
	stop         func()
	closeOnce    sync.Once
	frames       []Frame
	bytes        int
	lastActivity time.Time
	endedAt      time.Time
	writeGate    chan struct{}
}
type Manager struct {
	mu            sync.Mutex
	config        Config
	profiles      map[string]resolvedProfile
	sessions      map[string]*session
	store         MetadataStore
	resolveSecret func(context.Context, string, string, string) (Credential, error)
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	ownedStore    io.Closer
	closed        bool
	opening       int
	openingActors map[string]int
}

func New(options Options) (*Manager, error) {
	c := options.Config
	if len(c.Profiles) > 0 && c.APIUser == "" {
		return nil, errors.New("local terminal profiles require an explicit API user identity")
	}
	if c.MaxSessions == 0 {
		c.MaxSessions = 16
	}
	if c.MaxPerActor == 0 {
		c.MaxPerActor = min(4, c.MaxSessions)
	}
	if c.IdleSeconds == 0 {
		c.IdleSeconds = 900
	}
	if c.LeaseSeconds == 0 {
		c.LeaseSeconds = 60
	}
	if c.MaxSessions < 1 || c.MaxSessions > 64 || c.MaxPerActor < 1 || c.MaxPerActor > c.MaxSessions || c.IdleSeconds < 1 || c.LeaseSeconds < 1 || c.LeaseSeconds > 300 {
		return nil, errors.New("invalid terminal limits")
	}
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{config: c, profiles: map[string]resolvedProfile{}, sessions: map[string]*session{}, store: options.Store, resolveSecret: options.ResolveSecret, ctx: ctx, cancel: cancel, openingActors: map[string]int{}}
	apiUID, apiGID := uint32(os.Geteuid()), uint32(os.Getegid())
	if c.APIUser != "" {
		u, err := user.Lookup(c.APIUser)
		if err != nil {
			m.Close()
			return nil, errors.New("configured API user does not exist")
		}
		uid, e1 := strconv.ParseUint(u.Uid, 10, 32)
		gid, e2 := strconv.ParseUint(u.Gid, 10, 32)
		if e1 != nil || e2 != nil {
			m.Close()
			return nil, errors.New("invalid API identity")
		}
		apiUID, apiGID = uint32(uid), uint32(gid)
	}
	for _, p := range c.Profiles {
		if _, ok := m.profiles[p.ID]; ok {
			m.Close()
			return nil, errors.New("duplicate terminal profile")
		}
		rp, err := resolveProfile(p, apiUID, apiGID, c.EnableRoot, options.UnsafeSameUIDForTests, c.ProtectedPaths)
		if err != nil {
			m.Close()
			return nil, err
		}
		m.profiles[p.ID] = rp
	}
	// Resolve and validate every OS profile before creating durable runtime
	// state. A rejected identity must not leave a partially initialized store.
	if m.store == nil {
		store, err := NewFileStore(c.StateDir)
		if err != nil {
			cancel()
			return nil, err
		}
		m.store, m.ownedStore = store, store
	}
	m.wg.Add(1)
	go m.sweep()
	return m, nil
}

// ValidateConfig is a read-only policy preflight. It does not create a store,
// allocate a PTY, start a ticker, recover records or connect to an SSH server.
func ValidateConfig(config Config) error {
	if !filepath.IsAbs(config.StateDir) {
		return errors.New("terminal state directory must be absolute")
	}
	maxSessions := config.MaxSessions
	if maxSessions == 0 {
		maxSessions = 16
	}
	maxPerActor := config.MaxPerActor
	if maxPerActor == 0 {
		maxPerActor = min(4, maxSessions)
	}
	idle, lease := config.IdleSeconds, config.LeaseSeconds
	if idle == 0 {
		idle = 900
	}
	if lease == 0 {
		lease = 60
	}
	if maxSessions < 1 || maxSessions > 64 || maxPerActor < 1 || maxPerActor > maxSessions || idle < 1 || lease < 1 || lease > 300 {
		return errors.New("invalid terminal limits")
	}
	if len(config.Profiles) == 0 {
		return nil
	}
	if config.APIUser == "" {
		return errors.New("local terminal profiles require an explicit API user identity")
	}
	api, err := user.Lookup(config.APIUser)
	if err != nil {
		return errors.New("API user does not exist")
	}
	uid, err := strconv.ParseUint(api.Uid, 10, 32)
	if err != nil {
		return err
	}
	gid, err := strconv.ParseUint(api.Gid, 10, 32)
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, p := range config.Profiles {
		if ids[p.ID] {
			return errors.New("duplicate terminal profile")
		}
		ids[p.ID] = true
		if _, err := resolveProfile(p, uint32(uid), uint32(gid), config.EnableRoot, false, config.ProtectedPaths); err != nil {
			return err
		}
	}
	return nil
}
func resolveProfile(p LocalProfile, apiUID, apiGID uint32, enableRoot, unsafeTest bool, protected []string) (resolvedProfile, error) {
	if !safeID.MatchString(p.ID) || p.User == "" || !filepath.IsAbs(p.Shell) || !filepath.IsAbs(p.Home) || !filepath.IsAbs(p.WorkDir) {
		return resolvedProfile{}, errors.New("local terminal profile needs a trusted ID, user, absolute shell/home/work_dir")
	}
	u, err := user.Lookup(p.User)
	if err != nil {
		return resolvedProfile{}, errors.New("terminal OS identity does not exist")
	}
	uid, e1 := strconv.ParseUint(u.Uid, 10, 32)
	gid, e2 := strconv.ParseUint(u.Gid, 10, 32)
	if e1 != nil || e2 != nil {
		return resolvedProfile{}, errors.New("invalid terminal OS identity")
	}
	isRoot := uid == 0
	if err := validateProfileIdentity(uint32(uid), uint32(gid), apiUID, apiGID, enableRoot, unsafeTest); err != nil {
		return resolvedProfile{}, err
	}
	if !unsafeTest && os.Geteuid() != 0 {
		return resolvedProfile{}, errors.New("isolated local terminals must be launched by the privileged host executor")
	}
	shell, err := filepath.EvalSymlinks(p.Shell)
	if err != nil {
		return resolvedProfile{}, err
	}
	info, err := os.Stat(shell)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return resolvedProfile{}, errors.New("terminal shell is not a trusted executable")
	}
	p.Shell = shell
	for _, path := range []string{p.Home, p.WorkDir} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return resolvedProfile{}, errors.New("terminal home/workspace does not exist")
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return resolvedProfile{}, err
		}
		for _, blocked := range protected {
			relative, err := filepath.Rel(blocked, resolved)
			if err == nil && relative != ".." && !strings.HasPrefix(relative, "../") {
				return resolvedProfile{}, errors.New("terminal directory overlaps protected panel state")
			}
		}
	}
	return resolvedProfile{LocalProfile: p, uid: uint32(uid), gid: uint32(gid), root: isRoot}, nil
}

func validateProfileIdentity(uid, gid, apiUID, apiGID uint32, enableRoot, unsafeTest bool) error {
	if uid == 0 {
		if !enableRoot {
			return errors.New("root terminal profiles are disabled")
		}
		return nil
	}
	if gid == 0 {
		return errors.New("non-root local terminals cannot use the root group")
	}
	if !unsafeTest && (uid == apiUID || gid == apiGID) {
		return errors.New("local terminal must use an OS identity isolated from the API user/group")
	}
	return nil
}
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func dimensions(rows, cols int) (int, int, error) {
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	if rows < 1 || rows > 500 || cols < 1 || cols > 1000 {
		return 0, 0, failure("invalid_input", "Terminal dimensions are out of range", 400)
	}
	return rows, cols, nil
}

type trustedFlags struct{ shared, root bool }
type flagsKey struct{}

func withFlags(ctx context.Context, shared, root bool) context.Context {
	return context.WithValue(ctx, flagsKey{}, trustedFlags{shared: shared, root: root})
}
func (s *session) authorized(ctx context.Context, actorID string) bool {
	flags, _ := ctx.Value(flagsKey{}).(trustedFlags)
	return actorID != "" && (s.info.ActorID == actorID || flags.shared) && (!s.info.Root || flags.root)
}

func (m *Manager) Open(ctx context.Context, actorID string, in OpenInput, credential *Credential) (SessionInfo, error) {
	if actorID == "" {
		return SessionInfo{}, failure("unauthorized", "Authenticated actor required", 401)
	}
	rows, cols, err := dimensions(in.Rows, in.Cols)
	if err != nil {
		return SessionInfo{}, err
	}
	target, err := m.GetTarget(ctx, in.TargetID)
	if err != nil {
		return SessionInfo{}, err
	}
	// Credentials resolved by the panel belong to this exact target revision.
	// The immutable snapshot below is also the one used by the SSH dialer.
	if in.ExpectedRevision != 0 && in.ExpectedRevision != target.Revision {
		return SessionInfo{}, failure("conflict", "Terminal target changed after credentials were resolved", 409)
	}
	if target.Root && !in.RootAuthorized {
		return SessionInfo{}, failure("forbidden", "Root terminal requires a separate explicit root authorization", 403)
	}
	m.mu.Lock()
	active, owned := m.opening, m.openingActors[actorID]
	for _, s := range m.sessions {
		s.mu.Lock()
		if !s.info.Exited {
			active++
			if s.info.ActorID == actorID {
				owned++
			}
		}
		s.mu.Unlock()
	}
	if m.closed || active >= m.config.MaxSessions || owned >= m.config.MaxPerActor {
		m.mu.Unlock()
		return SessionInfo{}, failure("busy", "Terminal session budget is exhausted", 429)
	}
	m.opening++
	m.openingActors[actorID]++
	m.wg.Add(1)
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.opening--; m.openingActors[actorID]--; m.mu.Unlock(); m.wg.Done() }()
	now := time.Now().UTC()
	s := &session{info: SessionInfo{ID: newID(), ActorID: actorID, TargetID: target.ID, Type: target.Type, Root: target.Root, Rows: rows, Cols: cols, CreatedAt: now, ExpiresAt: now.Add(time.Duration(m.config.LeaseSeconds) * time.Second), ExitCode: -1}, lastActivity: now, frames: []Frame{}, writeGate: make(chan struct{}, 1)}
	if target.Type == "local" {
		err = m.openPTY(s, m.profiles[target.ID])
	} else {
		var cred Credential
		if credential != nil {
			cred = *credential
		} else if m.resolveSecret != nil {
			cred, err = m.resolveSecret(ctx, actorID, target.ID, target.SecretRef)
		} else {
			err = failure("missing_secret", "SSH credentials were not resolved by the trusted adapter", 422)
		}
		if err == nil {
			openCtx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(m.ctx, cancel)
			err = m.openSSH(openCtx, s, target, cred)
			stop()
			cancel()
		}
	}
	if err != nil {
		return SessionInfo{}, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		s.close("shutdown")
		return SessionInfo{}, errors.New("terminal manager is closed")
	}
	m.sessions[s.info.ID] = s
	m.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info, nil
}
func (s *session) append(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.info.Sequence++
	encoded := base64.StdEncoding.EncodeToString(data)
	s.frames = append(s.frames, Frame{Sequence: s.info.Sequence, Data: encoded})
	s.bytes += len(encoded)
	for (s.bytes > 88<<10 || len(s.frames) > 256) && len(s.frames) > 1 {
		s.bytes -= len(s.frames[0].Data)
		s.frames = s.frames[1:]
	}
}
func (s *session) close(reason string) {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.info.Reason = reason
		s.mu.Unlock()
		if s.stop != nil {
			s.stop()
		}
	})
}
func (s *session) exited(exit int) {
	s.mu.Lock()
	s.info.Exited, s.info.ExitCode = true, exit
	s.endedAt = time.Now().UTC()
	s.mu.Unlock()
}
func (m *Manager) lookup(ctx context.Context, actorID, id string) (*session, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, failure("closed", "Terminal manager is closed", 409)
	}
	if !ok {
		return nil, failure("not_found", "Terminal session not found", 404)
	}
	s.mu.Lock()
	allowed := s.authorized(ctx, actorID)
	s.mu.Unlock()
	if !allowed {
		return nil, failure("forbidden", "Terminal session access is not authorized", 403)
	}
	return s, nil
}
func (m *Manager) Info(id string) (SessionInfo, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return SessionInfo{}, failure("not_found", "Terminal session not found", 404)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info, nil
}
func (m *Manager) Read(ctx context.Context, actorID string, in Input) (ReadResult, error) {
	s, err := m.lookup(ctx, actorID, in.ID)
	if err != nil {
		return ReadResult{}, err
	}
	limit := in.Limit
	if limit == 0 {
		limit = 8
	}
	if limit < 1 || limit > 8 {
		return ReadResult{}, failure("invalid_input", "Read limit must be 1..8 frames", 400)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := ReadResult{SessionInfo: s.info, Frames: []Frame{}, Next: in.After}
	if len(s.frames) > 0 && in.After+1 < s.frames[0].Sequence {
		out.Truncated = true
	}
	for _, frame := range s.frames {
		if frame.Sequence > in.After {
			out.Frames = append(out.Frames, frame)
			out.Next = frame.Sequence
			if len(out.Frames) == limit {
				break
			}
		}
	}
	return out, nil
}
func (m *Manager) Input(ctx context.Context, actorID string, in Input) (SessionInfo, error) {
	data, err := base64.StdEncoding.DecodeString(in.Data)
	if err != nil || len(data) == 0 || len(data) > 16<<10 {
		return SessionInfo{}, failure("invalid_input", "Input must contain one to 16384 base64-encoded bytes", 400)
	}
	s, err := m.lookup(ctx, actorID, in.ID)
	if err != nil {
		return SessionInfo{}, err
	}
	select {
	case s.writeGate <- struct{}{}:
	case <-ctx.Done():
		return SessionInfo{}, ctx.Err()
	case <-m.ctx.Done():
		return SessionInfo{}, failure("closed", "Terminal manager is closed", 409)
	}
	defer func() { <-s.writeGate }()
	s.mu.Lock()
	if s.info.Exited {
		s.mu.Unlock()
		return SessionInfo{}, failure("closed", "Terminal has exited", 409)
	}
	writer := s.input
	s.lastActivity = time.Now().UTC()
	s.info.ExpiresAt = s.lastActivity.Add(time.Duration(m.config.LeaseSeconds) * time.Second)
	s.mu.Unlock()
	written := make(chan error, 1)
	go func() { _, err := writer.Write(data); written <- err }()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-written:
		if err != nil {
			return SessionInfo{}, failure("terminal_io", "Terminal input could not be delivered", 502)
		}
	case <-ctx.Done():
		s.close("input_canceled")
		<-written
		return SessionInfo{}, ctx.Err()
	case <-timer.C:
		s.close("input_timeout")
		<-written
		return SessionInfo{}, failure("terminal_io", "Terminal input backpressure exceeded its deadline", 504)
	case <-m.ctx.Done():
		s.close("shutdown")
		<-written
		return SessionInfo{}, failure("closed", "Terminal manager is closed", 409)
	}
	return m.Info(in.ID)
}
func (m *Manager) Resize(ctx context.Context, actorID string, in Input) (SessionInfo, error) {
	rows, cols, err := dimensions(in.Rows, in.Cols)
	if err != nil {
		return SessionInfo{}, err
	}
	s, err := m.lookup(ctx, actorID, in.ID)
	if err != nil {
		return SessionInfo{}, err
	}
	s.mu.Lock()
	resize := s.resize
	if s.info.Exited {
		s.mu.Unlock()
		return SessionInfo{}, failure("closed", "Terminal has exited", 409)
	}
	s.mu.Unlock()
	if err := resize(rows, cols); err != nil {
		return SessionInfo{}, err
	}
	s.mu.Lock()
	s.info.Rows, s.info.Cols = rows, cols
	s.mu.Unlock()
	return m.Info(in.ID)
}
func (m *Manager) Renew(ctx context.Context, actorID string, in Input) (SessionInfo, error) {
	s, err := m.lookup(ctx, actorID, in.ID)
	if err != nil {
		return SessionInfo{}, err
	}
	s.mu.Lock()
	if !s.info.Exited {
		s.info.ExpiresAt = time.Now().UTC().Add(time.Duration(m.config.LeaseSeconds) * time.Second)
	}
	info := s.info
	s.mu.Unlock()
	return info, nil
}
func (m *Manager) CloseSession(ctx context.Context, actorID string, in Input) (SessionInfo, error) {
	s, err := m.lookup(ctx, actorID, in.ID)
	if err != nil {
		return SessionInfo{}, err
	}
	s.close("closed")
	return m.Info(in.ID)
}
func (m *Manager) sweep() {
	defer m.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			m.mu.Lock()
			list := []*session{}
			for id, s := range m.sessions {
				s.mu.Lock()
				if s.info.Exited {
					if now.Sub(s.endedAt) > 5*time.Minute {
						delete(m.sessions, id)
					}
				} else if now.After(s.info.ExpiresAt) || now.Sub(s.lastActivity) > time.Duration(m.config.IdleSeconds)*time.Second || now.Sub(s.info.CreatedAt) > 8*time.Hour {
					list = append(list, s)
				}
				s.mu.Unlock()
			}
			m.mu.Unlock()
			for _, s := range list {
				s.close("expired")
			}
		}
	}
}
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancel()
	list := []*session{}
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.Unlock()
	for _, s := range list {
		s.close("shutdown")
	}
	m.wg.Wait()
	if m.ownedStore != nil {
		return m.ownedStore.Close()
	}
	return nil
}

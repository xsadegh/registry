package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"path"
	"slices"
	"sync"
	"time"

	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	settingsPath = "/dashboard/settings.json"
	statsPath    = "/dashboard/stats.json"
	maxEvents    = 200
)

type account struct {
	Username string    `json:"username"`
	Hash     []byte    `json:"hash"`
	Created  time.Time `json:"created"`
}

type token struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Hash    string    `json:"hash"`
	Hint    string    `json:"hint"`
	Push    bool      `json:"push"`
	Repos   []string  `json:"repos,omitempty"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires,omitzero"`
}

func (t *token) expired() bool {
	return !t.Expires.IsZero() && time.Now().After(t.Expires)
}

func (t *token) covers(repo string) bool {
	if len(t.Repos) == 0 {
		return true
	}
	for _, p := range t.Repos {
		if ok, _ := path.Match(p, repo); ok {
			return true
		}
	}
	return false
}

type settings struct {
	Secret  []byte          `json:"secret"`
	Account *account        `json:"account,omitempty"`
	Tokens  []*token        `json:"tokens"`
	Public  map[string]bool `json:"public"`
}

type event struct {
	Type  string    `json:"type"`
	Repo  string    `json:"repo,omitempty"`
	Ref   string    `json:"ref,omitempty"`
	Actor string    `json:"actor"`
	Size  int64     `json:"size,omitempty"`
	Time  time.Time `json:"time"`
}

type hour struct {
	Hour   int64 `json:"hour"`
	Pulls  int   `json:"pulls"`
	Pushes int   `json:"pushes"`
	Egress int64 `json:"egress"`
}

type gcRun struct {
	Time  time.Time `json:"time"`
	Freed int64     `json:"freed"`
	Blobs int       `json:"blobs"`
	By    string    `json:"by"`
}

type stats struct {
	Events   []event              `json:"events"`
	Hours    []hour               `json:"hours"`
	Usage    map[string]int64     `json:"usage"`
	LastUsed map[string]time.Time `json:"lastUsed"`
	GC       []gcRun              `json:"gc"`
}

type store struct {
	mu       sync.Mutex
	driver   storagedriver.StorageDriver
	settings settings
	stats    stats
	dirty    bool
	log      *zap.Logger
}

func openStore(lc fx.Lifecycle, d storagedriver.StorageDriver, log *zap.Logger) (*store, error) {
	ctx := context.Background()
	s := &store{driver: d, log: log.Named("activity")}
	if err := s.load(ctx, settingsPath, &s.settings); err != nil {
		return nil, err
	}
	if err := s.load(ctx, statsPath, &s.stats); err != nil {
		return nil, err
	}
	if s.settings.Public == nil {
		s.settings.Public = map[string]bool{}
	}
	if s.stats.Usage == nil {
		s.stats.Usage = map[string]int64{}
	}
	if s.stats.LastUsed == nil {
		s.stats.LastUsed = map[string]time.Time{}
	}
	if len(s.settings.Secret) == 0 {
		s.settings.Secret = make([]byte, 32)
		_, _ = rand.Read(s.settings.Secret)
		if err := s.saveSettings(); err != nil {
			return nil, err
		}
	}
	loop, cancel := context.WithCancel(context.Background())
	lc.Append(fx.StartStopHook(
		func() { go s.flushLoop(loop) },
		func() {
			cancel()
			s.flush()
		},
	))
	return s, nil
}

func (s *store) load(ctx context.Context, p string, v any) error {
	b, err := s.driver.GetContent(ctx, p)
	var nf storagedriver.PathNotFoundError
	if errors.As(err, &nf) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (s *store) saveSettings() error {
	b, err := json.MarshalIndent(s.settings, "", "  ")
	if err != nil {
		return err
	}
	return s.driver.PutContent(context.Background(), settingsPath, b)
}

func (s *store) flush() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	b, err := json.Marshal(s.stats)
	s.dirty = false
	s.mu.Unlock()
	if err == nil {
		err = s.driver.PutContent(context.Background(), statsPath, b)
	}
	if err != nil {
		s.log.Error("save stats", zap.Error(err))
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
}

func (s *store) flushLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.flush()
		}
	}
}

func (s *store) secret() []byte {
	return s.settings.Secret
}

func (s *store) hasAccount() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings.Account != nil
}

func (s *store) account() account {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settings.Account == nil {
		return account{}
	}
	return *s.settings.Account
}

func (s *store) createAccount(username string, hash []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settings.Account != nil {
		return errors.New("the account already exists")
	}
	s.settings.Account = &account{Username: username, Hash: hash, Created: time.Now().UTC()}
	return s.saveSettings()
}

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func (s *store) tokens() ([]token, map[string]time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]token, 0, len(s.settings.Tokens))
	for _, t := range s.settings.Tokens {
		out = append(out, *t)
	}
	slices.SortFunc(out, func(a, b token) int { return b.Created.Compare(a.Created) })
	return out, maps.Clone(s.stats.LastUsed)
}

func (s *store) addToken(t *token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings.Tokens = append(s.settings.Tokens, t)
	return s.saveSettings()
}

func (s *store) deleteToken(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings.Tokens = slices.DeleteFunc(s.settings.Tokens, func(t *token) bool { return t.ID == id })
	delete(s.stats.LastUsed, id)
	s.dirty = true
	return s.saveSettings()
}

func (s *store) tokenBySecret(secret string) *token {
	h := hashSecret(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.settings.Tokens {
		if t.Hash == h && !t.expired() {
			s.stats.LastUsed[t.ID] = time.Now().UTC()
			s.dirty = true
			c := *t
			return &c
		}
	}
	return nil
}

func (s *store) tokenByID(id string) *token {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.settings.Tokens {
		if t.ID == id && !t.expired() {
			c := *t
			return &c
		}
	}
	return nil
}

func (s *store) isPublic(repo string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings.Public[repo]
}

func (s *store) setPublic(repo string, public bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if public {
		s.settings.Public[repo] = true
	} else {
		delete(s.settings.Public, repo)
	}
	return s.saveSettings()
}

func (s *store) record(e event) {
	e.Time = time.Now().UTC()
	fields := []zap.Field{zap.String("repo", e.Repo), zap.String("ref", e.Ref), zap.String("actor", e.Actor)}
	if e.Size > 0 {
		fields = append(fields, zap.Int64("size", e.Size))
	}
	s.log.Info(e.Type, fields...)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Events = slices.Insert(s.stats.Events, 0, e)
	if len(s.stats.Events) > maxEvents {
		s.stats.Events = s.stats.Events[:maxEvents]
	}
	s.dirty = true
}

func (s *store) count(pulls, pushes int, egress int64) {
	now := time.Now().Unix() / 3600
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.stats.Hours); n == 0 || s.stats.Hours[n-1].Hour != now {
		s.stats.Hours = append(s.stats.Hours, hour{Hour: now})
	}
	h := &s.stats.Hours[len(s.stats.Hours)-1]
	h.Pulls += pulls
	h.Pushes += pushes
	h.Egress += egress
	s.stats.Hours = slices.DeleteFunc(s.stats.Hours, func(h hour) bool { return h.Hour <= now-7*24 })
	s.dirty = true
}

func (s *store) recordUsage(bytes int64) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stats.Usage[now.Format(time.DateOnly)] == bytes {
		return
	}
	s.stats.Usage[now.Format(time.DateOnly)] = bytes
	oldest := now.AddDate(0, 0, -31).Format(time.DateOnly)
	maps.DeleteFunc(s.stats.Usage, func(day string, _ int64) bool { return day < oldest })
	s.dirty = true
}

func (s *store) addGC(r gcRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.GC = slices.Insert(s.stats.GC, 0, r)
	if len(s.stats.GC) > 20 {
		s.stats.GC = s.stats.GC[:20]
	}
	s.dirty = true
}

func (s *store) snapshot() stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return stats{
		Events: slices.Clone(s.stats.Events),
		Hours:  slices.Clone(s.stats.Hours),
		Usage:  maps.Clone(s.stats.Usage),
		GC:     slices.Clone(s.stats.GC),
	}
}

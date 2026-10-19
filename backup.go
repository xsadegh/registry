package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/registry/storage"
	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/distribution/distribution/v3/registry/storage/driver/factory"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	backupDelay = 15 * time.Second
	settingsJob = ""
)

type backupState struct {
	Digest string    `json:"digest"`
	Tags   []string  `json:"tags"`
	Time   time.Time `json:"time"`
	Error  string    `json:"error,omitempty"`
}

type backup struct {
	enabled bool
	target  string
	reg     *registry
	store   *store
	driver  storagedriver.StorageDriver
	ns      distribution.Namespace
	log     *zap.Logger
	jobs    chan string
	mu      sync.Mutex
	timers  map[string]*time.Timer
	busy    atomic.Bool
}

func newBackup(lc fx.Lifecycle, cfg Config, reg *registry, st *store, log *zap.Logger) (*backup, error) {
	b := &backup{reg: reg, store: st, log: log.Named("backup"), jobs: make(chan string, 1024), timers: map[string]*time.Timer{}}
	if !cfg.Backup.enabled() {
		return b, nil
	}
	d, err := factory.Create(context.Background(), "s3", cfg.Backup.S3.params())
	if err != nil {
		return nil, err
	}
	ns, err := storage.NewRegistry(context.Background(), d, storage.EnableDelete)
	if err != nil {
		return nil, err
	}
	b.enabled, b.target, b.driver, b.ns = true, cfg.Backup.S3.url(), d, ns
	st.onSave = func() { b.schedule(settingsJob, 5*time.Second) }

	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.StartStopHook(
		func() {
			go b.loop(ctx)
			go b.enqueueAll(ctx)
		},
		cancel,
	))
	return b, nil
}

func (b *backup) schedule(job string, delay time.Duration) {
	if !b.enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.timers[job]; ok {
		t.Reset(delay)
		return
	}
	b.timers[job] = time.AfterFunc(delay, func() {
		b.mu.Lock()
		delete(b.timers, job)
		b.mu.Unlock()
		b.jobs <- job
	})
}

func (b *backup) enqueueAll(ctx context.Context) {
	if !b.enabled {
		return
	}
	err := b.reg.ns.(distribution.RepositoryEnumerator).Enumerate(ctx, func(name string) error {
		b.jobs <- name
		return nil
	})
	if err != nil && !notFound(err) {
		b.log.Error("list repositories", zap.Error(err))
	}
	b.jobs <- settingsJob
}

func (b *backup) running() bool {
	return b.busy.Load() || len(b.jobs) > 0
}

func (b *backup) loop(ctx context.Context) {
	changed := false
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-b.jobs:
			b.busy.Store(true)
			if job != settingsJob && b.repo(ctx, job) {
				changed = true
			}
			if len(b.jobs) > 0 {
				continue
			}
			b.settings(ctx)
			if changed {
				if err := storage.MarkAndSweep(ctx, b.driver, b.ns, storage.GCOpts{RemoveUntagged: true, Quiet: true}); err != nil {
					b.log.Error("remove old backups", zap.Error(err))
				}
				changed = false
			}
			b.busy.Store(false)
		}
	}
}

func (b *backup) repo(ctx context.Context, name string) bool {
	b.reg.gcLock.RLock()
	defer b.reg.gcLock.RUnlock()
	d, tags, err := b.reg.newest(ctx, name)
	if err == nil && d == "" {
		return false
	}
	changed := false
	if err == nil {
		changed, err = b.copy(ctx, name, d, tags)
	}
	if err != nil {
		b.log.Error("back up repository", zap.String("repo", name), zap.Error(err))
		b.store.setBackupError(name, err.Error())
		return false
	}
	b.store.setBackup(name, backupState{Digest: d.String(), Tags: tags, Time: time.Now().UTC()})
	if changed {
		b.log.Info("backed up", zap.String("repo", name), zap.Strings("tags", tags), zap.String("digest", d.String()))
	}
	return changed
}

func (b *backup) copy(ctx context.Context, name string, d digest.Digest, tags []string) (bool, error) {
	src, srcMs, err := b.reg.open(ctx, name)
	if err != nil {
		return false, err
	}
	dst, dstMs, err := openRepo(ctx, b.ns, name)
	if err != nil {
		return false, err
	}
	dstTags := dst.Tags(ctx)
	current, err := dstTags.All(ctx)
	if err != nil && !notFound(err) {
		return false, err
	}
	slices.Sort(current)
	if slices.Equal(current, tags) && b.tagged(ctx, dstTags, tags, d) {
		return false, nil
	}
	if err := copyManifest(ctx, src, dst, srcMs, dstMs, d); err != nil {
		return false, err
	}
	for _, t := range tags {
		if err := dstTags.Tag(ctx, t, v1.Descriptor{Digest: d}); err != nil {
			return false, err
		}
	}
	for _, t := range current {
		if !slices.Contains(tags, t) {
			if err := dstTags.Untag(ctx, t); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

func (b *backup) tagged(ctx context.Context, ts distribution.TagService, tags []string, d digest.Digest) bool {
	for _, t := range tags {
		desc, err := ts.Get(ctx, t)
		if err != nil || desc.Digest != d {
			return false
		}
	}
	return true
}

func copyManifest(ctx context.Context, src, dst distribution.Repository, srcMs, dstMs distribution.ManifestService, d digest.Digest) error {
	if ok, err := dstMs.Exists(ctx, d); err == nil && ok {
		return nil
	}
	m, err := srcMs.Get(ctx, d)
	if err != nil {
		return err
	}
	for _, ref := range m.References() {
		switch ref.MediaType {
		case v1.MediaTypeImageManifest, v1.MediaTypeImageIndex, dockerManifest, dockerList:
			err = copyManifest(ctx, src, dst, srcMs, dstMs, ref.Digest)
		default:
			err = copyBlob(ctx, src.Blobs(ctx), dst.Blobs(ctx), ref)
		}
		if err != nil {
			return err
		}
	}
	_, err = dstMs.Put(ctx, m)
	return err
}

func copyBlob(ctx context.Context, from, to distribution.BlobStore, desc v1.Descriptor) error {
	if _, err := to.Stat(ctx, desc.Digest); err == nil {
		return nil
	} else if !errors.Is(err, distribution.ErrBlobUnknown) {
		return err
	}
	rc, err := from.Open(ctx, desc.Digest)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	w, err := to.Create(ctx)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, rc); err != nil {
		_ = w.Cancel(ctx)
		return err
	}
	_, err = w.Commit(ctx, v1.Descriptor{Digest: desc.Digest, Size: desc.Size, MediaType: desc.MediaType})
	return err
}

func (b *backup) settings(ctx context.Context) {
	data, err := b.store.driver.GetContent(ctx, settingsPath)
	if err == nil {
		err = b.driver.PutContent(ctx, settingsPath, data)
	}
	if err != nil {
		b.log.Error("back up settings", zap.Error(err))
		return
	}
	b.store.setSettingsBackup(time.Now().UTC())
}

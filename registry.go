package main

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/handlers"
	"github.com/distribution/distribution/v3/registry/storage"
	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/distribution/distribution/v3/registry/storage/driver/factory"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/filesystem"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/s3-aws"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	root              = "/docker/registry/v2"
	dockerManifest    = "application/vnd.docker.distribution.manifest.v2+json"
	dockerList        = "application/vnd.docker.distribution.manifest.list.v2+json"
	dockerImageConfig = "application/vnd.docker.container.image.v1+json"
)

type layer struct {
	Cmd  string
	Size int64
}

type platform struct {
	Arch   string
	Size   int64
	Layers []layer
}

type tag struct {
	Name      string
	Digest    digest.Digest
	MediaType string
	Pushed    time.Time
	Created   time.Time
	Revision  string
	Platforms []platform
}

func (t *tag) Size() int64 {
	if len(t.Platforms) == 0 {
		return 0
	}
	return t.Platforms[0].Size
}

type repo struct {
	Name   string
	Tags   []*tag
	Size   int64
	Pushed time.Time
}

type scan struct {
	Repos      []*repo
	Total      int64
	Referenced int64
	Uploads    int64
	Blobs      int
	Loose      int
	Time       time.Time
}

func (s *scan) repo(name string) *repo {
	for _, r := range s.Repos {
		if r.Name == name {
			return r
		}
	}
	return nil
}

type cached struct {
	mediaType string
	payload   []byte
}

type registry struct {
	driver storagedriver.StorageDriver
	ns     distribution.Namespace
	app    *handlers.App
	cache  sync.Map
	scanMu sync.Mutex
	last   *scan
	stale  atomic.Bool
	gcLock sync.RWMutex
	gcBusy atomic.Bool
	log    *zap.Logger
}

func openDriver(cfg Config) (storagedriver.StorageDriver, error) {
	return factory.Create(context.Background(), cfg.Storage.Driver, cfg.driverParams())
}

func newRegistry(lc fx.Lifecycle, cfg Config, d storagedriver.StorageDriver, st *store, log *zap.Logger) (*registry, error) {
	ctx := context.Background()
	ns, err := storage.NewRegistry(ctx, d, storage.EnableDelete)
	if err != nil {
		return nil, err
	}
	conf := &configuration.Configuration{}
	conf.Storage = configuration.Storage{
		cfg.Storage.Driver: cfg.driverParams(),
		"delete":           configuration.Parameters{"enabled": true},
	}
	conf.HTTP.Secret = hex.EncodeToString(st.secret())
	conf.HTTP.RelativeURLs = true
	conf.Catalog.MaxEntries = 1000
	conf.Log.AccessLog.Disabled = true
	conf.Log.Level = "error"
	app := handlers.NewApp(ctx, conf)
	lc.Append(fx.StopHook(app.Shutdown))
	return &registry{driver: d, ns: ns, app: app, log: log.Named("registry")}, nil
}

func notFound(err error) bool {
	var nf storagedriver.PathNotFoundError
	var ru distribution.ErrRepositoryUnknown
	return errors.As(err, &nf) || errors.As(err, &ru)
}

func (r *registry) snapshot(ctx context.Context) (*scan, error) {
	r.scanMu.Lock()
	defer r.scanMu.Unlock()
	if r.last != nil && !r.stale.Load() && time.Since(r.last.Time) < time.Minute {
		return r.last, nil
	}
	r.stale.Store(false)
	s, err := r.scan(ctx)
	if err != nil {
		r.stale.Store(true)
		return nil, err
	}
	r.last = s
	return s, nil
}

func (r *registry) fresh(ctx context.Context) (*scan, error) {
	r.stale.Store(true)
	return r.snapshot(ctx)
}

func (r *registry) scan(ctx context.Context) (*scan, error) {
	s := &scan{Time: time.Now()}
	sizes := map[digest.Digest]int64{}
	err := r.driver.Walk(ctx, root, func(fi storagedriver.FileInfo) error {
		if fi.IsDir() {
			return nil
		}
		p := fi.Path()
		if rest, ok := strings.CutPrefix(p, root+"/blobs/"); ok {
			parts := strings.Split(rest, "/")
			if len(parts) == 4 && parts[3] == "data" {
				sizes[digest.NewDigestFromEncoded(digest.Algorithm(parts[0]), parts[2])] = fi.Size()
				s.Total += fi.Size()
			}
		} else if strings.Contains(p, "/_uploads/") {
			s.Uploads += fi.Size()
		}
		return nil
	})
	if err != nil && !notFound(err) {
		return nil, err
	}
	s.Blobs = len(sizes)

	marked := map[digest.Digest]bool{}
	err = r.ns.(distribution.RepositoryEnumerator).Enumerate(ctx, func(name string) error {
		rp, err := r.loadRepo(ctx, name, sizes, marked)
		if err != nil {
			return err
		}
		s.Repos = append(s.Repos, rp)
		return nil
	})
	if err != nil && !notFound(err) {
		return nil, err
	}
	for d, size := range sizes {
		if marked[d] {
			s.Referenced += size
		} else {
			s.Loose++
		}
	}
	return s, nil
}

func (r *registry) open(ctx context.Context, name string) (distribution.Repository, distribution.ManifestService, error) {
	return openRepo(ctx, r.ns, name)
}

func openRepo(ctx context.Context, ns distribution.Namespace, name string) (distribution.Repository, distribution.ManifestService, error) {
	named, err := reference.WithName(name)
	if err != nil {
		return nil, nil, err
	}
	repository, err := ns.Repository(ctx, named)
	if err != nil {
		return nil, nil, err
	}
	ms, err := repository.Manifests(ctx)
	return repository, ms, err
}

func tagLink(name, tag string) string {
	return path.Join(root, "repositories", name, "_manifests/tags", tag, "current/link")
}

func (r *registry) newest(ctx context.Context, name string) (digest.Digest, []string, error) {
	repository, _, err := r.open(ctx, name)
	if err != nil {
		return "", nil, err
	}
	ts := repository.Tags(ctx)
	names, err := ts.All(ctx)
	if err != nil {
		if notFound(err) {
			return "", nil, nil
		}
		return "", nil, err
	}
	var best digest.Digest
	var bestTime time.Time
	digests := map[string]digest.Digest{}
	for _, n := range names {
		d, err := ts.Get(ctx, n)
		if err != nil {
			continue
		}
		digests[n] = d.Digest
		if fi, err := r.driver.Stat(ctx, tagLink(name, n)); err == nil && fi.ModTime().After(bestTime) {
			best, bestTime = d.Digest, fi.ModTime()
		}
	}
	var tags []string
	for n, d := range digests {
		if d == best {
			tags = append(tags, n)
		}
	}
	slices.Sort(tags)
	return best, tags, nil
}

func (r *registry) loadRepo(ctx context.Context, name string, sizes map[digest.Digest]int64, marked map[digest.Digest]bool) (*repo, error) {
	repository, ms, err := r.open(ctx, name)
	if err != nil {
		return nil, err
	}
	ts := repository.Tags(ctx)
	names, err := ts.All(ctx)
	if err != nil && !notFound(err) {
		return nil, err
	}
	rp := &repo{Name: name}
	own := map[digest.Digest]bool{}
	for _, n := range names {
		d, err := ts.Get(ctx, n)
		if err != nil {
			continue
		}
		t := &tag{Name: n, Digest: d.Digest}
		if fi, err := r.driver.Stat(ctx, tagLink(name, n)); err == nil {
			t.Pushed = fi.ModTime()
		}
		if err := r.describe(ctx, repository, ms, t, own); err != nil {
			r.log.Warn("read tag", zap.String("repo", name), zap.String("tag", n), zap.Error(err))
			continue
		}
		rp.Tags = append(rp.Tags, t)
		if t.Pushed.After(rp.Pushed) {
			rp.Pushed = t.Pushed
		}
	}
	slices.SortFunc(rp.Tags, func(a, b *tag) int {
		return cmp.Or(b.Pushed.Compare(a.Pushed), strings.Compare(a.Name, b.Name))
	})
	for d := range own {
		rp.Size += sizes[d]
		marked[d] = true
	}
	return rp, nil
}

func (r *registry) manifest(ctx context.Context, ms distribution.ManifestService, d digest.Digest) (string, []byte, error) {
	if v, ok := r.cache.Load(d); ok {
		c := v.(cached)
		return c.mediaType, c.payload, nil
	}
	m, err := ms.Get(ctx, d)
	if err != nil {
		return "", nil, err
	}
	mt, payload, err := m.Payload()
	if err != nil {
		return "", nil, err
	}
	r.cache.Store(d, cached{mt, payload})
	return mt, payload, nil
}

func (r *registry) blob(ctx context.Context, bs distribution.BlobStore, d digest.Digest) ([]byte, error) {
	if v, ok := r.cache.Load(d); ok {
		return v.(cached).payload, nil
	}
	b, err := bs.Get(ctx, d)
	if err != nil {
		return nil, err
	}
	r.cache.Store(d, cached{payload: b})
	return b, nil
}

func isIndex(mediaType string) bool {
	return mediaType == v1.MediaTypeImageIndex || mediaType == dockerList
}

func (r *registry) describe(ctx context.Context, repository distribution.Repository, ms distribution.ManifestService, t *tag, own map[digest.Digest]bool) error {
	mt, payload, err := r.manifest(ctx, ms, t.Digest)
	if err != nil {
		return err
	}
	own[t.Digest] = true
	t.MediaType = mt
	if !isIndex(mt) {
		p, img, err := r.image(ctx, repository, payload, own)
		if err != nil {
			return err
		}
		t.Platforms = []platform{p}
		t.setImage(img)
		return nil
	}
	var idx v1.Index
	if err := json.Unmarshal(payload, &idx); err != nil {
		return err
	}
	for _, m := range idx.Manifests {
		own[m.Digest] = true
		_, cp, err := r.manifest(ctx, ms, m.Digest)
		if err != nil {
			continue
		}
		p, img, err := r.image(ctx, repository, cp, own)
		if err != nil || m.Platform == nil || m.Platform.OS == "unknown" {
			continue
		}
		p.Arch = archOf(m.Platform.Architecture, m.Platform.Variant)
		t.Platforms = append(t.Platforms, p)
		t.setImage(img)
	}
	return nil
}

func (t *tag) setImage(img v1.Image) {
	if !t.Created.IsZero() {
		return
	}
	if img.Created != nil {
		t.Created = *img.Created
	}
	t.Revision = img.Config.Labels[v1.AnnotationRevision]
}

func archOf(arch, variant string) string {
	if variant != "" {
		return arch + "/" + variant
	}
	return arch
}

func (r *registry) image(ctx context.Context, repository distribution.Repository, payload []byte, own map[digest.Digest]bool) (platform, v1.Image, error) {
	var m v1.Manifest
	var img v1.Image
	if err := json.Unmarshal(payload, &m); err != nil {
		return platform{}, img, err
	}
	own[m.Config.Digest] = true
	p := platform{Size: m.Config.Size, Arch: "artifact"}
	for _, l := range m.Layers {
		own[l.Digest] = true
		p.Size += l.Size
	}
	if m.Config.MediaType == v1.MediaTypeImageConfig || m.Config.MediaType == dockerImageConfig {
		if b, err := r.blob(ctx, repository.Blobs(ctx), m.Config.Digest); err == nil {
			_ = json.Unmarshal(b, &img)
			p.Arch = archOf(img.Architecture, img.Variant)
		}
	}
	i := 0
	for _, h := range img.History {
		l := layer{Cmd: cleanCmd(h.CreatedBy)}
		if !h.EmptyLayer && i < len(m.Layers) {
			l.Size = m.Layers[i].Size
			i++
		}
		p.Layers = append(p.Layers, l)
	}
	for ; i < len(m.Layers); i++ {
		p.Layers = append(p.Layers, layer{Cmd: m.Layers[i].Digest.String(), Size: m.Layers[i].Size})
	}
	return p, img, nil
}

func cleanCmd(s string) string {
	s = strings.TrimSuffix(strings.TrimSpace(s), "# buildkit")
	if _, after, ok := strings.Cut(s, "/bin/sh -c #(nop) "); ok {
		return strings.TrimSpace(after)
	}
	if strings.HasPrefix(s, "|") || strings.HasPrefix(s, "/bin/sh -c ") {
		if _, after, ok := strings.Cut(s, "/bin/sh -c "); ok {
			return "RUN " + strings.TrimSpace(after)
		}
	}
	return strings.TrimSpace(strings.Replace(s, "RUN /bin/sh -c ", "RUN ", 1))
}

func (r *registry) deleteManifest(ctx context.Context, name string, d digest.Digest) ([]string, error) {
	repository, ms, err := r.open(ctx, name)
	if err != nil {
		return nil, err
	}
	ts := repository.Tags(ctx)
	tags, err := ts.Lookup(ctx, v1.Descriptor{Digest: d})
	if err != nil {
		return nil, err
	}
	for _, t := range tags {
		if err := ts.Untag(ctx, t); err != nil {
			return nil, err
		}
	}
	r.stale.Store(true)
	if err := ms.Delete(ctx, d); err != nil && !notFound(err) {
		return nil, err
	}
	return tags, nil
}

func (r *registry) deleteRepo(ctx context.Context, name string) error {
	named, err := reference.WithName(name)
	if err != nil {
		return err
	}
	r.stale.Store(true)
	return r.ns.(distribution.RepositoryRemover).Remove(ctx, named)
}

func (r *registry) collect(ctx context.Context) (int64, int, error) {
	r.gcLock.Lock()
	defer r.gcLock.Unlock()
	before, err := r.fresh(ctx)
	if err != nil {
		return 0, 0, err
	}
	if err := storage.MarkAndSweep(ctx, r.driver, r.ns, storage.GCOpts{RemoveUntagged: true, Quiet: true}); err != nil {
		return 0, 0, err
	}
	after, err := r.fresh(ctx)
	if err != nil {
		return 0, 0, err
	}
	return before.Total - after.Total, before.Blobs - after.Blobs, nil
}

package webui

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

const assetLockKey int64 = 0x5745424153534554 // WEBASSET; publish, renew and GC share this order.
var hashedAssetName = regexp.MustCompile(`-[A-Za-z0-9_-]{8,}\.[A-Za-z0-9.]+$`)

func publicAsset(name string) bool {
	return fs.ValidPath(name) && strings.HasPrefix(name, "assets/") && hashedAssetName.MatchString(path.Base(name))
}

type SharedOptions struct {
	Lease, Interval, Retention                 time.Duration
	KeepBuilds, MaxBuilds, MaxFiles            int
	MaxFileBytes, MaxBuildBytes, MaxTotalBytes int64
	Logger                                     *slog.Logger
}

type publicFile struct {
	name, hash string
	body       []byte
}

// SharedAssets publishes only public hashed assets from this binary. A local
// miss can then read the exact URL emitted by another build during rollout.
type SharedAssets struct {
	db         *store.DB
	opts       SharedOptions
	buildID    string
	files      []publicFile
	mu         sync.Mutex
	readyUntil atomic.Pointer[time.Time]
}

func NewSharedAssets(db *store.DB, embedded fs.FS, opts SharedOptions) (*SharedAssets, error) {
	if opts.Lease <= 0 {
		opts.Lease = 2 * time.Minute
	}
	if opts.Interval <= 0 {
		opts.Interval = 30 * time.Second
	}
	if opts.Retention <= 0 {
		opts.Retention = 7 * 24 * time.Hour
	}
	if opts.KeepBuilds <= 0 {
		opts.KeepBuilds = 3
	}
	if opts.MaxBuilds <= 0 {
		opts.MaxBuilds = 64
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = 65536
	}
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = 16 << 20
	}
	if opts.MaxBuildBytes <= 0 {
		opts.MaxBuildBytes = 128 << 20
	}
	if opts.MaxTotalBytes <= 0 {
		opts.MaxTotalBytes = 1 << 30
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &SharedAssets{db: db, opts: opts}
	root, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	var size int64
	err = fs.WalkDir(root, "assets", func(name string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && name == "assets" {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() || !publicAsset(name) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > opts.MaxFileBytes {
			return fmt.Errorf("console asset exceeds file limit: %s", name)
		}
		body, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		size += int64(len(body))
		if size > opts.MaxBuildBytes || len(s.files) >= opts.MaxFiles {
			return errors.New("console build exceeds asset limits")
		}
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])
		s.files = append(s.files, publicFile{name, digest, body})
		fmt.Fprintf(h, "%s\x00%s\x00", name, digest)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.buildID = hex.EncodeToString(h.Sum(nil))
	return s, nil
}

func (s *SharedAssets) Ready() bool {
	until := s.readyUntil.Load()
	return until != nil && time.Now().Before(*until)
}
func (s *SharedAssets) markReady(start time.Time) {
	until := start.Add(s.opts.Lease)
	s.readyUntil.Store(&until)
}

// collect runs under assetLockKey. A lease, seven-day retention and the newest
// three builds independently protect references; only unreferenced blobs go.
func (s *SharedAssets) collect(ctx context.Context, tx *gorm.DB) error {
	g := store.G(ctx, tx)
	if err := g.Exec(collectBuildsSQL, s.opts.Retention.Seconds(), s.opts.KeepBuilds).Error; err != nil {
		return err
	}
	return g.Exec(collectAssetsSQL).Error
}

// collectBuildsSQL drops the builds past their lease and their retention
// (seconds) that are not among the newest kept ones.
const collectBuildsSQL = `DELETE FROM web_asset_builds WHERE lease_until<=clock_timestamp()
	AND last_seen<clock_timestamp()-make_interval(secs=>?)
	AND build_id NOT IN (SELECT build_id FROM web_asset_builds ORDER BY published_at DESC,build_id DESC LIMIT ?)`

// collectAssetsSQL drops the blobs no build refers to.
const collectAssetsSQL = `DELETE FROM web_assets a WHERE NOT EXISTS(SELECT 1 FROM web_asset_build_files f WHERE f.path=a.path)`

// lockAssets takes assetLockKey for the rest of the transaction.
func lockAssets(g *gorm.DB) error {
	return g.Exec(`SELECT pg_advisory_xact_lock(?)`, assetLockKey).Error
}

// publishBuildSQL registers the build with a lease of ? seconds, or renews
// it.
const publishBuildSQL = `INSERT INTO web_asset_builds(build_id,lease_until) VALUES(?,clock_timestamp()+make_interval(secs=>?))
	ON CONFLICT(build_id) DO UPDATE SET lease_until=EXCLUDED.lease_until,last_seen=clock_timestamp()`

// renewBuild extends the lease of build by secs seconds.
func renewBuild(g *gorm.DB, build string, secs float64) *gorm.DB {
	return g.Model(&model.WebAssetBuild{}).Where("build_id = ?", build).
		Updates(map[string]any{
			"lease_until": gorm.Expr("clock_timestamp()+make_interval(secs=>?)", secs),
			"last_seen":   gorm.Expr("clock_timestamp()"),
		})
}

// putAssetsSQL stores the files of a build in one statement and one round
// trip, whatever their number: the paths, hashes and bodies are an array
// parameter each, which unnest turns back into rows, written in the files'
// order (WITH ORDINALITY). Each blob is stored, or the stored one kept when
// it has the same hash; a retained immutable path is never replaced with
// different bytes, so put returns no row for it. Each path put returns is
// listed among the files of the build (the last parameter). The statement
// returns the paths put returned: a file missing from them is a collision,
// which fails the publication (its transaction rolls back what the
// statement wrote).
const putAssetsSQL = `WITH put AS (
	INSERT INTO web_assets(path,sha256,body,size)
	SELECT path,sha256,body,octet_length(body) FROM unnest(?::text[],?::text[],?::bytea[]) WITH ORDINALITY AS f(path,sha256,body,o) ORDER BY o
	ON CONFLICT(path) DO UPDATE SET sha256=web_assets.sha256 WHERE web_assets.sha256=EXCLUDED.sha256 RETURNING path
), listed AS (
	INSERT INTO web_asset_build_files(build_id,path) SELECT ?,path FROM put ON CONFLICT DO NOTHING
)
SELECT path FROM put`

// putAssets runs putAssetsSQL for files, of build.
func putAssets(g *gorm.DB, build string, files []publicFile) *gorm.DB {
	paths, hashes, bodies := make(store.Strings, len(files)), make(store.Strings, len(files)), make(store.Array[store.Bytes], len(files))
	for i, f := range files {
		paths[i], hashes[i], bodies[i] = f.name, f.hash, f.body
	}
	return g.Raw(putAssetsSQL, paths, hashes, bodies, build)
}

// storeUsage selects the bytes and files of the shared assets and the
// number of builds.
func storeUsage(g *gorm.DB) *gorm.DB {
	return g.Model(&model.WebAsset{}).Select("COALESCE(sum(size),0), count(*), (SELECT count(*) FROM web_asset_builds)")
}

func (s *SharedAssets) Publish(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := time.Now()
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		g := store.G(ctx, tx)
		if err := lockAssets(g); err != nil {
			return err
		}
		if err := s.collect(ctx, tx); err != nil {
			return err
		}
		if err := g.Exec(publishBuildSQL, s.buildID, s.opts.Lease.Seconds()).Error; err != nil {
			return err
		}
		var put []string
		if err := putAssets(g, s.buildID, s.files).Scan(&put).Error; err != nil {
			return fmt.Errorf("immutable console asset collision or write failure: %w", err)
		}
		for _, f := range s.files {
			if !slices.Contains(put, f.name) {
				return fmt.Errorf("immutable console asset collision or write failure (%s): %w", f.name, sql.ErrNoRows)
			}
		}
		var size int64
		var files, builds int
		if err := storeUsage(g).Row().Scan(&size, &files, &builds); err != nil {
			return err
		}
		if size > s.opts.MaxTotalBytes || files > s.opts.MaxFiles || builds > s.opts.MaxBuilds {
			return errors.New("shared console assets reached capacity; retained or active builds cannot be evicted")
		}
		return nil
	})
	if err == nil {
		s.markReady(start)
	}
	return err
}

func (s *SharedAssets) renew(ctx context.Context) error {
	s.mu.Lock()
	start := time.Now()
	found := false
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		g := store.G(ctx, tx)
		if err := lockAssets(g); err != nil {
			return err
		}
		res := renewBuild(g, s.buildID, s.opts.Lease.Seconds())
		if res.Error != nil {
			return res.Error
		}
		found = res.RowsAffected == 1
		return s.collect(ctx, tx)
	})
	if err == nil && found {
		s.markReady(start)
	}
	s.mu.Unlock()
	if err == nil && !found {
		return s.Publish(ctx)
	}
	return err
}

// AssetLeaseTask is the node task that publishes this node's console build
// and renews its lease.
const AssetLeaseTask = "core.webui.asset_lease"

// RegisterTasks registers the asset lease task: on every node from start,
// once at once (readiness waits for it) and then every Interval.
func (s *SharedAssets) RegisterTasks(tasks core.Tasks) error {
	return tasks.Register(core.TaskSpec{
		Name:  AssetLeaseTask,
		Title: core.LocalizedText{"en": "Renew the console asset lease", "zh": "续期前端资源租约"},
		Description: core.LocalizedText{
			"en": "Publishes this node's console build to the shared asset store and renews its lease, so every node can serve the assets of every running build.",
			"zh": "把本节点的控制台构建发布到共享资源表并续期租约，让每个节点都能提供所有在运行版本的前端资源。",
		},
		Scope:      core.TaskNode,
		Every:      s.opts.Interval,
		Timeout:    30 * time.Second,
		RunOnStart: true,
		Handler: func(ctx context.Context, _ core.TaskRun) (core.TaskResult, error) {
			return core.TaskResult{}, s.converge(ctx)
		},
	})
}

// converge publishes the build until it is ready, then renews its lease.
func (s *SharedAssets) converge(ctx context.Context) error {
	if s.readyUntil.Load() == nil {
		return s.Publish(ctx)
	}
	return s.renew(ctx)
}

func (s *SharedAssets) Read(ctx context.Context, name string) ([]byte, error) {
	if !publicAsset(name) {
		return nil, fs.ErrNotExist
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var a model.WebAsset
	err := store.G(ctx, s.db.Gorm).Select("body").Take(&a, "path = ?", name).Error
	if store.IsNoRows(err) {
		return nil, fs.ErrNotExist
	}
	return a.Body, err
}

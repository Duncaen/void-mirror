package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/exp/slog"

	"github.com/hashicorp/hcl/v2"

	"github.com/gammazero/workerpool"

	"github.com/Duncaen/go-xbps/repo/repodata"

	"github.com/void-linux/void-mirror/config"
)

var (
	conffile = flag.String("conffile", "config.hcl", "configuration file path")
)

func updateRequestConditions(req *http.Request, resp *http.Response) {
	if value := resp.Header.Get("Last-Modified"); value != "" {
		if date, err := http.ParseTime(value); err != nil {
			slog.Warn("failed to parse Last-Modified", "error", err)
		} else {
			req.Header.Set("If-Modified-Since", date.Format(http.TimeFormat))
		}
	}
	if value := resp.Header.Get("ETag"); value != "" {
		req.Header.Set("If-None-Match", value)
	}
}

type Stagedata struct {
	config *config.RepositoryConfig
	req    *http.Request
	index  index
}

func NewStagedata(config *config.RepositoryConfig) (*Stagedata, error) {
	url := config.Upstream.JoinPath(fmt.Sprintf("%s-stagedata", config.Architecture))
	req, err := http.NewRequest(http.MethodGet, url.String(), nil)
	if err != nil {
		return nil, err
	}
	file := fmt.Sprintf("%s-stagedata", config.Architecture)
	idx, err := readRepodata(filepath.Join(config.Destination, file))
	if err != nil {
		return nil, err
	}
	return &Stagedata{config: config, req: req, index: idx}, nil
}

type tmpfile struct {
	file *os.File
}

func Tempfile(dir, pattern string) (*tmpfile, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return &tmpfile{file}, nil
}
func (t *tmpfile) Remove() error {
	if t.file == nil {
		return nil
	}
	t.file.Close()
	return os.Remove(t.file.Name())
}
func (t *tmpfile) Commit(path string) error {
	if err := t.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(t.file.Name(), path); err != nil {
		return err
	}
	t.file = nil
	return nil
}

func fetchRepodata(ctx context.Context, req *http.Request, dir string, file string) (*http.Response, error) {
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		return resp, err
	}
	defer resp.Body.Close()

	slog.Debug("request", slog.Group("req", "url", req.URL), slog.Group("resp", "status", resp.StatusCode))

	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}

	pattern := fmt.Sprintf(".%s.*", file)
	tmpfile, err := Tempfile(dir, pattern)
	if err != nil {
		slog.Error("creating tempfile failed", "error", err)
		return resp, err
	}
	defer tmpfile.Remove()

	tmpfilename := tmpfile.file.Name()
	_, err = io.Copy(tmpfile.file, resp.Body)
	if err != nil {
		slog.Error("writing repodata file failed", "error", err)
		return resp, err
	}
	if err := resp.Body.Close(); err != nil {
		slog.Error("closing response body failed", "error", err)
	}

	// set the modtime on the temporary file
	if value := resp.Header.Get("Last-Modified"); value != "" {
		if date, err := time.Parse(http.TimeFormat, value); err == nil {
			if err := os.Chtimes(tmpfilename, time.Now(), date); err != nil {
				slog.Error("changing mod time failed", "path", tmpfilename, "error", err)
			}
		}
	}

	// everything fine, rename the temporary file
	destfile := filepath.Join(dir, file)
	if err := tmpfile.Commit(destfile); err != nil {
		slog.Error("renaming tempfile", "oldpath", tmpfilename, "newpath", destfile, "error", err)
		return resp, err
	}

	return resp, nil
}

type digest []byte

func (h *digest) UnmarshalPlist(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	data, err := hex.DecodeString(s)
	if err != nil {
		return fmt.Errorf("could not decode digest", err)
	}
	*h = data
	return nil
}

// pkg is a package from {repo,stage}data with the metadata fields we care for.
type pkg struct {
	Pkgver string `plist:"pkgver"`
	Arch   string `plist:"architecture"`
	SHA256 digest `plist:"filename-sha256"`
}

func (pkg pkg) Filename() string {
	return fmt.Sprintf("%s.%s.xbps", pkg.Pkgver, pkg.Arch)
}

// index is the repository index
type index map[string]*pkg

type indexDiff struct {
	Added   []*pkg
	Deleted []*pkg
}

func (idx index) Diff(other index) indexDiff {
	var d indexDiff
	for name, newpkg := range other {
		oldpkg, ok := idx[name]
		if !ok {
			d.Added = append(d.Added, newpkg)
		} else if newpkg.Pkgver != oldpkg.Pkgver {
			d.Added = append(d.Added, newpkg)
			d.Deleted = append(d.Deleted, oldpkg)
		}
	}
	for name, pkg := range idx {
		if _, ok := other[name]; !ok {
			d.Deleted = append(d.Deleted, pkg)
		}
	}
	return d
}

func readRepodata(path string) (index, error) {
	rd, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rd.Close()
	var result struct {
		Index index `repodata:"index.plist"`
	}
	dec := repodata.NewDecoder(rd)
	err = dec.Decode(&result)
	if err != nil {
		return nil, err
	}
	return result.Index, nil
}

func (data *Stagedata) Update(ctx context.Context) (*indexDiff, error) {
	file := fmt.Sprintf("%s-stagedata", data.config.Architecture)
	resp, err := fetchRepodata(ctx, data.req, data.config.Destination, file)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, nil
	case http.StatusOK:
		path := filepath.Join(data.config.Destination, file)
		index, err := readRepodata(path)
		if err != nil {
			slog.Error("invalid stagedata", "path", path, "error", err)
			if err := os.Remove(path); err != nil {
				if !os.IsNotExist(err) {
					slog.Error("could not delete invalid stagedata", "path", path, "error", err)
				}
			}
			return nil, nil
		}
		diff := data.index.Diff(index)
		data.index = index
		updateRequestConditions(data.req, resp)
		return &diff, nil
	case http.StatusNotFound:
		if data.index == nil {
			return nil, nil
		}
		// 404 for stagedata is different from repodata, we delete the file
		// and return an empty index.
		if err := os.Remove(filepath.Join(data.config.Destination, file)); err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}
		}
		data.index = nil
		return &indexDiff{}, nil
	default:
		slog.Error("unexpected status code", "url", data.req.URL, "status", resp.StatusCode)
		return nil, nil
	}
}

type Repodata struct {
	config *config.RepositoryConfig
	req    *http.Request
	index  index
}

func NewRepodata(config *config.RepositoryConfig) (*Repodata, error) {
	url := config.Upstream.JoinPath(fmt.Sprintf("%s-repodata", config.Architecture))
	req, err := http.NewRequest(http.MethodGet, url.String(), nil)
	if err != nil {
		return nil, err
	}
	file := fmt.Sprintf("%s-repodata", config.Architecture)
	idx, err := readRepodata(filepath.Join(config.Destination, file))
	if err != nil {
		return nil, err
	}
	return &Repodata{config: config, req: req, index: idx}, nil
}

func (data *Repodata) Update(ctx context.Context) (*indexDiff, error) {
	file := fmt.Sprintf("%s-repodata", data.config.Architecture)
	resp, err := fetchRepodata(ctx, data.req, data.config.Destination, file)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, nil
	case http.StatusOK:
		path := filepath.Join(data.config.Destination, file)
		index, err := readRepodata(path)
		if err != nil {
			slog.Error("invalid repodata", "path", path, "error", err)
			if err := os.Remove(path); err != nil {
				if !os.IsNotExist(err) {
					slog.Error("could not delete invalid repodata", "path", path, "error", err)
				}
			}
			return nil, nil
		}
		diff := data.index.Diff(index)
		data.index = index
		updateRequestConditions(data.req, resp)
		return &diff, nil
	case http.StatusNotFound:
		slog.Error("repodata not found", "url", data.req.URL, "status", resp.StatusCode)
		return nil, nil
	default:
		slog.Error("unexpected status code", "url", data.req.URL, "status", resp.StatusCode)
		return nil, nil
	}
}

type Repository struct {
	Config    *config.RepositoryConfig
	Repodata  *Repodata
	Stagedata *Stagedata
	ticker    *time.Ticker
	req       *http.Request
	downloads map[*DownloadFile]struct{}
	files     map[string]struct{}
	obsolete  map[string]time.Time
	results   chan DownloadResult
	ctx       context.Context
}

func (r *Repository) queue(download DownloadFile) {
	dl := &download
	r.downloads[dl] = struct{}{}
	wp.Submit(func() {
		err := dl.Download(r.ctx, r.req)
		r.results <- DownloadResult{dl, err}
	})
}

func (r *Repository) queuePkg(pkg *pkg) {
	binpkg := pkg.Filename()
	r.queue(DownloadFile{
		URL:       r.Config.Upstream.JoinPath(binpkg),
		Filename:  binpkg,
		Directory: r.Config.Destination,
		SHA256:    pkg.SHA256,
	})
	sigfile := binpkg + ".sig"
	r.queue(DownloadFile{
		URL:       r.Config.Upstream.JoinPath(sigfile),
		Filename:  sigfile,
		Directory: r.Config.Destination,
	})
}

func NewRepository(ctx context.Context, config *config.RepositoryConfig) (*Repository, error) {
	r := &Repository{
		Config: config,
		files:     make(map[string]struct{}),
		obsolete:  make(map[string]time.Time),
		downloads: make(map[*DownloadFile]struct{}),
		results:   make(chan DownloadResult),
		ctx:       ctx,
	}
	var err error
	r.Repodata, err = NewRepodata(config)
	if err != nil {
		return nil, err
	}
	r.Stagedata, err = NewStagedata(config)
	if err != nil {
		return nil, err
	}
	r.req, err = http.NewRequest(http.MethodGet, config.Upstream.String(), nil)
	if err != nil {
		return nil, err
	}
	r.ticker = time.NewTicker(*config.Interval)
	if err := os.MkdirAll(config.Destination, 0755); err != nil {
		return nil, err
	}
	for _, pkg := range r.Repodata.index {
		binpkg := pkg.Filename()
		if _, err := os.Stat(filepath.Join(config.Destination, binpkg)); err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}
			r.queue(DownloadFile{
				URL:       config.Upstream.JoinPath(binpkg),
				Filename:  binpkg,
				Directory: config.Destination,
				SHA256:    pkg.SHA256,
			})
		}
		sigfile := binpkg + ".sig"
		if _, err := os.Stat(filepath.Join(config.Destination, sigfile)); err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}
			r.queue(DownloadFile{
				URL:       config.Upstream.JoinPath(sigfile),
				Filename:  sigfile,
				Directory: config.Destination,
			})
		}
		r.files[binpkg] = struct{}{}
		r.files[sigfile] = struct{}{}
	}
	return r, nil
}

func (r *Repository) update(ctx context.Context) error {
	repoDiff, err := r.Repodata.Update(ctx)
	if err != nil {
		return err
	}
	stageDiff, err := r.Stagedata.Update(ctx)
	if err != nil {
		return err
	}
	if stageDiff != nil {
		for _, added := range stageDiff.Added {
			r.queuePkg(added)
		}
		now := time.Now()
		for _, deleted := range stageDiff.Deleted {
			filename := deleted.Filename()
			r.obsolete[filename] = now
			r.obsolete[filename+".sig"] = now
		}
	}
	if repoDiff != nil {
		for _, added := range repoDiff.Added {
			r.queuePkg(added)
			binpkg := added.Filename()
			// packages may be removed from stage and marked as obsolete, undo that
			delete(r.obsolete, binpkg)
			delete(r.obsolete, binpkg + ".sig")
		}
		now := time.Now()
		for _, deleted := range repoDiff.Deleted {
			binpkg := deleted.Filename()
			r.obsolete[binpkg] = now
			r.obsolete[binpkg+".sig"] = now
		}
	}
	return nil
}

func (r *Repository) Run(ctx context.Context) error {
	if err := r.update(ctx); err != nil {
		return err
	}
	for {
		select {
		case _ = <-r.ticker.C:
			if err := r.update(ctx); err != nil {
				return err
			}
		case _ = <-r.results:
			// XXX: requeue failed downloads
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type DownloadResult struct {
	DownloadFile *DownloadFile
	err          error
}

type DownloadFile struct {
	URL       *url.URL
	Filename  string
	Directory string
	SHA256    []byte
}

func (dl *DownloadFile) Download(ctx context.Context, req *http.Request) (err error) {
	req = req.Clone(ctx)
	req.URL = dl.URL
	slog.Debug("downloading", slog.Group("req", "url", req.URL))
	var resp *http.Response
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	defer func() {
		attr := []any{
				slog.Group("req", "url", req.URL),
				slog.Group("resp", "status", resp.StatusCode),
		}
		if err != nil {
			attr = append(attr, slog.Any("error", err))
			slog.Info("download failed", attr...)
		} else {
			slog.Info("download finished", attr...)
		}
	}()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil
	default:
		slog.Error("unexpected status code", slog.Group("req", "url", req.URL), slog.Group("resp", "status", resp.Status))
		return nil
	case http.StatusOK:
		// fallthrough
	}

	var rd io.Reader
	rd = resp.Body
	var hash hash.Hash
	if len(dl.SHA256) != 0 {
		hash = sha256.New()
		rd = io.TeeReader(rd, hash)
	}

	pattern := fmt.Sprintf(".%s.*", dl.Filename)
	tmpfile, err := Tempfile(dl.Directory, pattern)
	if err != nil {
		slog.Error("could not create tmp file", "directory", dl.Directory, "error", err)
		return err
	}
	defer tmpfile.Remove()
	tmpfilename := tmpfile.file.Name()

	_, err = io.Copy(tmpfile.file, rd)
	if err != nil {
		slog.Error("writing tmpfile failed", "path", tmpfilename, "error", err)
		return err
	}
	if err = resp.Body.Close(); err != nil {
		slog.Error("closing response body failed", "error", err)
		err = nil
	}

	if hash != nil {
		sum := hash.Sum(nil)
		if !bytes.Equal(sum[:], dl.SHA256) {
			slog.Error("hash mismatch", "expected", hex.EncodeToString(dl.SHA256), "got", hex.EncodeToString(sum))
			return fmt.Errorf("hash mismatch")
		}
	}

	// set the modtime on the temporary file
	if value := resp.Header.Get("Last-Modified"); value != "" {
		var date time.Time
		if date, err = time.Parse(http.TimeFormat, value); err == nil {
			if err := os.Chtimes(tmpfilename, time.Now(), date); err != nil {
				slog.Error("changing mod time failed", "path", tmpfilename, "error", err)
				err = nil
			}
		}
	}

	// everything fine, rename the temporary file
	destfile := filepath.Join(dl.Directory, dl.Filename)
	if err = tmpfile.Commit(filepath.Join(dl.Directory, dl.Filename)); err != nil {
		slog.Error("renaming tempfile", "oldpath", tmpfilename, "newpath", destfile, "error", err)
		return err
	}
	return nil
}

var wp *workerpool.WorkerPool

func main() {
	flag.Parse()
	opts := &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}
	textHandler := slog.NewTextHandler(os.Stdout, opts)
	slog.SetDefault(slog.New(textHandler))

	var conf config.Config
	diags := conf.Load(*conffile)
	if diags != nil {
		for _, diag := range diags {
			switch diag.Severity {
			case hcl.DiagError:
				slog.Error(diag.Summary, "detail", diag.Detail, "subject", diag.Subject)
			case hcl.DiagWarning:
				slog.Warn(diag.Summary, "detail", diag.Detail, "subject", diag.Subject)
			}
		}
	}
	if diags.HasErrors() {
		os.Exit(1)
	}
	wp = workerpool.New(conf.Jobs)

	repos := []*Repository{}
	g, ctx := errgroup.WithContext(context.Background())
	for _, conf := range conf.Repositories {
		repo, err := NewRepository(ctx, conf)
		if err != nil {
			slog.Error("initializing repository failed", "error", err)
			os.Exit(1)
		}
		g.Go(func() error {
			return repo.Run(ctx)
		})
		repos = append(repos, repo)
	}

	err := g.Wait()
	if err != nil {
		slog.Error("something went wrong", "error", err)
	}
	wp.Stop()
}

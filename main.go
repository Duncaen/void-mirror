package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/exp/slog"
	"golang.org/x/sync/errgroup"

	"github.com/hashicorp/hcl/v2"

	"github.com/gammazero/workerpool"

	"github.com/carlmjohnson/requests"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Duncaen/go-xbps/repo/repodata"

	"github.com/void-linux/void-mirror/config"
	"github.com/void-linux/void-mirror/reqextra"
)

var (
	conffile = flag.String("conffile", "config.hcl", "configuration file path")
	listenaddr = flag.String("listen", ":9998", "listen address")
)

var (
	namespace = "void_mirror"
	download_bytes_total = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "download_bytes_total",
			Help:      "Bytes downloaded (total)",
		},
	)
	responses_total = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "response_total",
			Help:      "Total number of responses by status code",
		},
		[]string{"code"},
	)
	queue_running = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "queue_running",
			Help:      "Number of queue jobs currently running",
		},
	)
	queue_workers = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "queue_workers",
			Help:      "Number of workers",
		},
	)
)

var (
	transport = requests.LogTransport(http.DefaultClient.Transport, requestLogger)
)

func requestLogger(req *http.Request, res *http.Response, err error, d time.Duration) {
	slog.Debug("request",
		slog.Group("req",
			"url", req.URL,
			"method", req.Method,
		),
		slog.Group("resp",
			"status", res.StatusCode,
			"length", res.ContentLength,
		),
		"duration", d,
		"error", err,
	)
	responses_total.WithLabelValues(fmt.Sprintf("%d", res.StatusCode)).Add(1)
	download_bytes_total.Add(float64(res.ContentLength))
}

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
	ETag string
	LastModified string
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

type digest []byte

func (h *digest) UnmarshalPlist(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	data, err := hex.DecodeString(s)
	if err != nil {
		return fmt.Errorf("could not decode digest: %v", err)
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
	pattern := fmt.Sprintf(".%s-stagedata.*", data.config.Architecture)
	url := data.config.Upstream.JoinPath(file)
	var tmpfile string
	err := requests.URL(url.String()).
		Transport(transport).
		Header("If-Modified-Since", data.LastModified).
		Header("If-None-Match", data.ETag).
		CheckStatus(http.StatusOK).
		Handle(requests.ChainHandlers(
			reqextra.CopyCacheHeaders(&data.ETag, &data.LastModified),
			reqextra.ToTemp(data.config.Destination, pattern, &tmpfile),
		)).Fetch(ctx)
	if err != nil {
		if tmpfile != "" {
			os.Remove(tmpfile)
		}
		if requests.HasStatusErr(err, http.StatusNotFound) {
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
		} else if requests.HasStatusErr(err, http.StatusNotModified) {
			return nil, nil
		}
		return nil, err
	}

	path := filepath.Join(data.config.Destination, file)
	index, err := readRepodata(tmpfile)
	if err != nil {
		slog.Error("invalid stagedata", "path", path, "error", err)
		if err := os.Remove(path); err != nil {
			if !os.IsNotExist(err) {
				slog.Error("could not delete invalid stagedata", "path", path, "error", err)
			}
		}
		os.Remove(tmpfile)
		return nil, err
	}
	diff := data.index.Diff(index)
	data.index = index
	if err := os.Rename(tmpfile, path); err != nil {
		return &diff, err
	}
	return &diff, nil
}

type Repodata struct {
	config *config.RepositoryConfig
	req    *http.Request
	index  index
	ETag string
	LastModified string
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
	pattern := fmt.Sprintf(".%s-repodata.*", data.config.Architecture)
	url := data.config.Upstream.JoinPath(file)
	var tmpfile string
	err := requests.URL(url.String()).
		Transport(transport).
		Handle(reqextra.ToTemp(data.config.Destination, pattern, &tmpfile)).
		Fetch(ctx)
	if err != nil {
		if tmpfile != "" {
			os.Remove(tmpfile)
		}
		if requests.HasStatusErr(err, http.StatusNotModified) {
			return nil, nil
		}
		return nil, err
	}
	path := filepath.Join(data.config.Destination, file)
	index, err := readRepodata(tmpfile)
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
	if err := os.Rename(tmpfile, path); err != nil {
		return &diff, err
	}
	return &diff, nil
}

type Repository struct {
	Config    *config.RepositoryConfig
	Repodata  *Repodata
	Stagedata *Stagedata
	ticker    *time.Ticker
	req       *http.Request
	files     map[string]struct{}
	obsolete  map[string]time.Time
	ctx       context.Context
}

func (r *Repository) queue(req *requests.Builder) {
	wp.Submit(func() {
		queue_running.Inc()
		defer queue_running.Dec()
		url, err := req.URL()
		if err != nil {
			slog.Error("url error", "error", err)
			return
		}
		slog.Info("downloading", "url", url)
		err = req.Fetch(r.ctx)
		if err != nil {
			slog.Error("donwloading", "error", err)
		}
	})
}

func (r *Repository) queuePkg(pkg *pkg) error {
	binpkg := pkg.Filename()
	url := r.Config.Upstream.JoinPath(binpkg)
	path := filepath.Join(r.Config.Destination, binpkg)
	r.queue(requests.URL(url.String()).
		Transport(transport).
		Handle(reqextra.Sha256Verify(pkg.SHA256, reqextra.ToFileAtomic(path))))
	return nil
}

func (r *Repository) queueSig(pkg *pkg) error {
	sigfile := pkg.Filename() + ".sig"
	path := filepath.Join(r.Config.Destination, sigfile)
	url := r.Config.Upstream.JoinPath(sigfile)
	r.queue(requests.
		URL(url.String()).
		Handle(reqextra.ToFileAtomic(path)))
	return nil
}

func NewRepository(ctx context.Context, config *config.RepositoryConfig) (*Repository, error) {
	r := &Repository{
		Config: config,
		files:     make(map[string]struct{}),
		obsolete:  make(map[string]time.Time),
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
			if err := r.queuePkg(pkg); err != nil {
				return nil, err
			}
		}
		sigfile := binpkg + ".sig"
		if _, err := os.Stat(filepath.Join(config.Destination, sigfile)); err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}
			if err := r.queueSig(pkg); err != nil {
				return nil, err
			}
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
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

var wp *workerpool.WorkerPool

type collector struct {
	queueWaiting *prometheus.Desc
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.queueWaiting
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(c.queueWaiting, prometheus.GaugeValue, float64(wp.WaitingQueueSize()))
}

func main() {
	flag.Parse()
	opts := &slog.HandlerOptions{
		AddSource: true,
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
	queue_workers.Set(float64(conf.Jobs))

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
	prometheus.MustRegister(&collector{
		queueWaiting: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "queue_waiting"),
			"Queue waiting size",
			nil,
			nil,
		),
	})
	prometheus.MustRegister(download_bytes_total)
	prometheus.MustRegister(responses_total)
	prometheus.MustRegister(queue_running)
	prometheus.MustRegister(queue_workers)

	http.Handle("/metrics", promhttp.Handler())
	g.Go(func() error {
		return http.ListenAndServe(*listenaddr, nil)
	})

	err := g.Wait()
	if err != nil {
		slog.Error("something went wrong", "error", err)
	}
	wp.Stop()
}

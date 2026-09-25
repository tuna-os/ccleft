// Command ccleft reports REMAINING usage for AI coding-agent accounts.
//
//	ccleft probe [--json] [--home DIR ...] [--provider p,...] [--config FILE]
//	ccleft serve [--addr :9464] [--refresh 5m] [--home DIR ...] [--config FILE]
//	ccleft version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/tuna-os/ccleft"
)

type multi []string

func (m *multi) String() string { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*m = append(*m, s)
		}
	}
	return nil
}

type common struct {
	homes     multi
	providers multi
	config    string
	noEnv     bool
	agyPath   string
	endpoints multi
}

func (c *common) register(fs *flag.FlagSet) {
	fs.Var(&c.homes, "home", "agent home directory to probe (repeatable; default: $HOME)")
	fs.Var(&c.providers, "provider", "only these providers (repeatable or comma-separated): "+providerList())
	fs.StringVar(&c.config, "config", "", "YAML config listing homes/sources")
	fs.BoolVar(&c.noEnv, "no-env", false, "do not probe API-key providers from this process's environment")
	fs.StringVar(&c.agyPath, "agy", "", "path to the agy binary (default: agy on PATH)")
	fs.Var(&c.endpoints, "endpoint", "override a provider base URL, provider=URL (repeatable; for mocks and regional endpoints)")
}

func providerList() string {
	var s []string
	for _, p := range ccleft.Providers {
		s = append(s, string(p))
	}
	return strings.Join(s, ",")
}

func (c *common) plan(cfg *Config) (plan, error) {
	pl := plan{envProviders: !c.noEnv}
	for _, s := range c.providers {
		p, err := ccleft.ParseProvider(s)
		if err != nil {
			return pl, err
		}
		pl.providers = append(pl.providers, p)
	}
	if cfg != nil {
		pl.homes = cfg.Homes
		pl.extra = cfg.Sources
		if cfg.EnvProviders != nil && !c.noEnv {
			pl.envProviders = *cfg.EnvProviders
		}
	}
	for _, h := range c.homes {
		pl.homes = append(pl.homes, HomeConfig{Path: h})
	}
	if len(pl.homes) == 0 && len(pl.extra) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return pl, fmt.Errorf("no --home given and $HOME unresolvable: %w", err)
		}
		pl.homes = []HomeConfig{{Path: home}}
	}
	return pl, nil
}

func (c *common) client(cfg *Config) *ccleft.Client {
	p := &ccleft.Prober{AgyPath: c.agyPath}
	if p.AgyPath == "" && cfg != nil {
		p.AgyPath = cfg.AgyPath
	}
	for _, e := range c.endpoints {
		if k, v, ok := strings.Cut(e, "="); ok {
			if pr, err := ccleft.ParseProvider(k); err == nil {
				if p.Endpoints == nil {
					p.Endpoints = map[ccleft.Provider]string{}
				}
				p.Endpoints[pr] = v
			}
		}
	}
	cl := ccleft.NewClient(p)
	if cfg != nil && len(cfg.MinInterval) > 0 {
		cl.MinInterval = map[ccleft.Provider]time.Duration{}
		for k, v := range cfg.MinInterval {
			if pr, err := ccleft.ParseProvider(k); err == nil {
				cl.MinInterval[pr] = v
			}
		}
	}
	return cl
}

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "probe":
		err = runProbe(os.Args[2:], os.Stdout)
	case "serve":
		err = runServe(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("ccleft", ccleft.Version)
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccleft:", err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `ccleft — how much AI coding-agent quota is LEFT (the opposite of ccusage)

Usage:
  ccleft probe [--json] [--home DIR ...] [--provider p,...] [--config FILE]
  ccleft serve [--addr :9464] [--refresh 5m] [--home DIR ...] [--config FILE]
  ccleft version

Providers: `+providerList()+`
ccleft is read-only: it never refreshes or writes credential files.
`)
}

func loadCfg(path string) (*Config, error) {
	if path == "" {
		return nil, nil
	}
	return loadConfig(path)
}

// Output is the JSON document printed by probe --json and served on /readings.
type Output struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Readings    []ccleft.Reading `json:"readings"`
}

func runProbe(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	var c common
	c.register(fs)
	asJSON := fs.Bool("json", false, "print JSON")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall deadline")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadCfg(c.config)
	if err != nil {
		return err
	}
	pl, err := c.plan(cfg)
	if err != nil {
		return err
	}
	srcs, err := buildSources(pl)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	rs := c.client(cfg).GetAll(ctx, srcs)
	if rs == nil {
		rs = []ccleft.Reading{}
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(Output{GeneratedAt: time.Now().UTC(), Readings: rs})
	}
	printTable(stdout, rs)
	return nil
}

func printTable(w io.Writer, rs []ccleft.Reading) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tACCOUNT\tSTATE\tPLAN\tWINDOW\tLEFT\tRESETS\tHOMES")
	if len(rs) == 0 {
		fmt.Fprintln(tw, "(no providers detected)\t\t\t\t\t\t\t")
	}
	// Causes/messages are long; printed inside the table they widened the
	// STATE column for every row. They go below the table, numbered.
	var notes []string
	for _, r := range rs {
		state := string(r.State)
		if r.Stale {
			state += " (stale)"
		}
		if r.Cause != "" && (r.Stale || r.State != ccleft.StateOK) {
			notes = append(notes, fmt.Sprintf("[%d] %s %s: %s: %s", len(notes)+1, r.Provider, r.Account, r.Cause, oneLine(r.Message)))
			state += fmt.Sprintf(" [%d]", len(notes))
		}
		homes := strings.Join(r.Homes, ",")
		if len(r.Windows) == 0 {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t-\t-\t-\t%s\n", r.Provider, r.Account, state, r.Plan, homes)
			continue
		}
		for i, win := range r.Windows {
			p, a, s, pl, h := string(r.Provider), r.Account, state, r.Plan, homes
			if i > 0 {
				p, a, s, pl, h = "", "", "", "", ""
			}
			name := win.ID
			if !win.Binding {
				name += "*"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p, a, s, pl, name, left(win), resets(win), h)
		}
	}
	tw.Flush()
	fmt.Fprintln(w, "* = informational window (does not decide the account state)")
	for _, n := range notes {
		fmt.Fprintln(w, n)
	}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

func left(w ccleft.Window) string {
	switch {
	case w.Unit == ccleft.UnitPercent && w.RemainingPct != nil:
		return fmt.Sprintf("%.0f%%", *w.RemainingPct)
	case w.Remaining != nil && w.Limit != nil:
		return fmt.Sprintf("%.2f/%.0f %s", *w.Remaining, *w.Limit, w.Unit)
	case w.Remaining != nil:
		return fmt.Sprintf("%.2f %s", *w.Remaining, w.Unit)
	case w.RemainingPct != nil:
		return fmt.Sprintf("%.0f%%", *w.RemainingPct)
	}
	return "?"
}

func resets(w ccleft.Window) string {
	if w.ResetsAt == nil {
		return "-"
	}
	d := time.Until(*w.ResetsAt).Round(time.Minute)
	if d < 0 {
		return w.ResetsAt.Format(time.RFC3339)
	}
	return fmt.Sprintf("%s (in %s)", w.ResetsAt.Format("2006-01-02 15:04Z"), d)
}

// server holds the latest snapshot.
type server struct {
	mu     sync.RWMutex
	last   Output
	err    error
	client *ccleft.Client
}

func (s *server) set(o Output, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.last = o
	}
	s.err = err
}

func (s *server) get() (Output, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last, s.err
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readings", func(w http.ResponseWriter, r *http.Request) {
		o, _ := s.get()
		if o.Readings == nil {
			o.Readings = []ccleft.Reading{}
		}
		if p := r.URL.Query().Get("provider"); p != "" {
			var f []ccleft.Reading
			for _, x := range o.Readings {
				if string(x.Provider) == p {
					f = append(f, x)
				}
			}
			o.Readings = f
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(o)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		o, _ := s.get()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = ccleft.WritePrometheus(w, o.Readings)
		if s.client != nil {
			_ = ccleft.WriteUpstreamMetrics(w, s.client.UpstreamCounts())
		}
		if !o.GeneratedAt.IsZero() {
			fmt.Fprintf(w, "# HELP ccleft_last_refresh_timestamp_seconds Unix time of the last refresh cycle.\n# TYPE ccleft_last_refresh_timestamp_seconds gauge\nccleft_last_refresh_timestamp_seconds %d\n", o.GeneratedAt.Unix())
		}
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		o, err := s.get()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if o.GeneratedAt.IsZero() {
			http.Error(w, "no refresh completed yet", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	return mux
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var c common
	c.register(fs)
	addr := fs.String("addr", "", "listen address (default :9464)")
	refresh := fs.Duration("refresh", 0, "refresh interval (default 5m)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadCfg(c.config)
	if err != nil {
		return err
	}
	if *addr == "" {
		*addr = ":9464"
		if cfg != nil && cfg.Addr != "" {
			*addr = cfg.Addr
		}
	}
	if *refresh == 0 {
		*refresh = 5 * time.Minute
		if cfg != nil && cfg.Refresh > 0 {
			*refresh = cfg.Refresh
		}
	}
	pl, err := c.plan(cfg)
	if err != nil {
		return err
	}
	client := c.client(cfg)
	client.Jitter = 0.1
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// One line per upstream call (HTTP request / agy run) — cache hits and
	// backoff are silent — so the load on each quota endpoint is auditable
	// from the logs as well as from ccleft_upstream_requests_total.
	client.OnUpstream = func(src ccleft.Source, r ccleft.Reading, d time.Duration) {
		attrs := []any{"provider", r.Provider, "account", r.Account, "state", r.State, "took", d.Round(time.Millisecond).String()}
		if r.Cause != "" {
			attrs = append(attrs, "cause", r.Cause)
		}
		if r.RetryAt != nil {
			attrs = append(attrs, "retry_after", time.Until(*r.RetryAt).Round(time.Second).String())
		}
		log.Info("upstream", attrs...)
	}
	srv := &server{client: client}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cycle := func() {
		srcs, err := buildSources(pl)
		if err != nil {
			log.Error("building sources", "err", err)
			srv.set(Output{}, err)
			return
		}
		cctx, cancel := context.WithTimeout(ctx, *refresh)
		defer cancel()
		rs := client.GetAll(cctx, srcs)
		srv.set(Output{GeneratedAt: time.Now().UTC(), Readings: rs}, nil)
		summary := map[string]int{}
		for _, r := range rs {
			summary[string(r.Provider)+":"+string(r.State)]++
		}
		keys := make([]string, 0, len(summary))
		for k := range summary {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		log.Info("refreshed", "sources", len(srcs), "readings", len(rs), "states", strings.Join(keys, " "))
	}

	hs := &http.Server{Addr: *addr, Handler: srv.routes(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("ccleft serving", "addr", *addr, "refresh", refresh.String())

	go func() {
		cycle()
		t := time.NewTicker(*refresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cycle()
			}
		}
	}()

	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return hs.Shutdown(sctx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

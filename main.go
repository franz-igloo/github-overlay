package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

type server struct {
	client     *client
	bots       map[string]bool
	closedDays int

	fetchMu sync.Mutex // serializes fetches
	mu      sync.RWMutex
	snap    *Snapshot
}

func (s *server) refresh() {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	res, err := s.client.fetch(s.closedDays)
	now := time.Now().UTC().Format(time.RFC3339)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		log.Printf("refresh: %v", err)
		if s.snap == nil {
			s.snap = &Snapshot{PRs: map[string]*PR{}, Sections: map[string][]string{}, Bundles: map[int][]string{}}
		}
		// Keep the last good data and surface the error.
		s.snap.Error = err.Error()
		return
	}
	snap := buildSnapshot(res, s.bots)
	snap.FetchedAt = now
	if len(res.Errors) > 0 {
		snap.Error = res.Errors[0]
	}
	s.snap = snap
}

func (s *server) handlePRs(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	empty := s.snap == nil
	s.mu.RUnlock()
	if empty || r.URL.Query().Get("refresh") == "1" {
		s.refresh()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.snap)
}

type serveOpts struct {
	addr       string
	interval   time.Duration
	closedDays int
	bots       string
}

// serveFlags registers the server flags on fs. start and serve share them so
// start can forward its arguments to the background serve process verbatim.
func serveFlags(fs *flag.FlagSet) *serveOpts {
	o := &serveOpts{}
	fs.StringVar(&o.addr, "addr", "prs.localhost:7777", "listen address")
	fs.DurationVar(&o.interval, "interval", 60*time.Second, "GitHub poll interval")
	fs.IntVar(&o.closedDays, "closed-days", 7, "show PRs merged/closed in the last N days")
	fs.StringVar(&o.bots, "bots", "igloo-code-reviewer,chatgpt-codex-connector,codex,copilot-pull-request-reviewer",
		"comma-separated logins to treat as bots (in addition to GitHub App accounts)")
	return o
}

// listen binds addr. A "localhost" or "*.localhost" host binds both loopback
// addresses: macOS resolves *.localhost to ::1 only, while browsers resolve it
// to 127.0.0.1 themselves.
func listen(addr string) ([]net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if h := strings.ToLower(host); h != "localhost" && !strings.HasSuffix(h, ".localhost") {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		return []net.Listener{l}, nil
	}
	l4, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return nil, err
	}
	ls := []net.Listener{l4}
	if l6, err := net.Listen("tcp", net.JoinHostPort("::1", port)); err == nil {
		ls = append(ls, l6)
	}
	return ls, nil
}

func serve(o *serveOpts, pidFile bool) error {
	botSet := map[string]bool{}
	for _, b := range strings.Split(o.bots, ",") {
		if b = strings.TrimSpace(strings.ToLower(b)); b != "" {
			botSet[b] = true
		}
	}
	s := &server{client: newClient(), bots: botSet, closedDays: o.closedDays}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/prs", s.handlePRs)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	srv := &http.Server{Handler: mux}
	ls, err := listen(o.addr)
	if err != nil {
		return err
	}

	if pidFile {
		if err := writeState(state{PID: os.Getpid(), Addr: o.addr}); err != nil {
			return err
		}
		defer removeState()
	}

	go func() {
		s.refresh()
		for range time.Tick(o.interval) {
			s.refresh()
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("github-overlay listening on http://%s", o.addr)
	errc := make(chan error, len(ls))
	for _, l := range ls {
		go func() { errc <- srv.Serve(l) }()
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Printf("github-overlay stopped")
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags)
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

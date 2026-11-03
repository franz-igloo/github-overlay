package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const usage = `github-overlay — local dashboard for your GitHub pull requests

Usage:
  github-overlay <command> [flags]

Commands:
  start     Start the server in the background
  stop      Stop the background server
  restart   Stop, then start with the given flags
  status    Show whether the server is running and where
  open      Open the dashboard in the browser (starts the server if needed)
  logs      Print the server log (-f to follow)
  serve     Run the server in the foreground

Server flags (start, restart, serve, open):
  -addr string         listen address (default "prs.localhost:7777")
  -interval duration   GitHub poll interval (default 1m0s)
  -closed-days int     show PRs merged/closed in the last N days (default 7)
  -bots string         extra logins to treat as bots, comma-separated

State and logs live in %s
`

type state struct {
	PID  int    `json:"pid"`
	Addr string `json:"addr"`
}

func stateDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "github-overlay")
}

func statePath() string { return filepath.Join(stateDir(), "server.json") }
func logPath() string   { return filepath.Join(stateDir(), "server.log") }

func writeState(s state) error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(s)
	return os.WriteFile(statePath(), b, 0o644)
}

func removeState() { os.Remove(statePath()) }

// readState returns the running server's state, or nil if none is running.
// A state file left behind by a dead process is cleaned up.
func readState() *state {
	b, err := os.ReadFile(statePath())
	if err != nil {
		return nil
	}
	var s state
	if json.Unmarshal(b, &s) != nil || s.PID <= 0 || syscall.Kill(s.PID, 0) != nil {
		removeState()
		return nil
	}
	return &s
}

func healthy(addr string) bool {
	c := &http.Client{Timeout: time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Printf(usage, stateDir())
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "start":
		return cmdStart(rest)
	case "stop":
		return cmdStop()
	case "restart":
		if err := cmdStop(); err != nil {
			return err
		}
		return cmdStart(rest)
	case "status":
		return cmdStatus()
	case "open":
		return cmdOpen(rest)
	case "logs":
		return cmdLogs(rest)
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		o := serveFlags(fs)
		fs.Parse(rest)
		if s := readState(); s != nil {
			return fmt.Errorf("a background server is already running (pid %d on %s); run `github-overlay stop` first", s.PID, s.Addr)
		}
		return serve(o, false)
	case "_daemon": // internal: the detached process spawned by start
		fs := flag.NewFlagSet("_daemon", flag.ExitOnError)
		o := serveFlags(fs)
		fs.Parse(rest)
		return serve(o, true)
	case "help", "-h", "--help":
		fmt.Printf(usage, stateDir())
		return nil
	default:
		fmt.Fprintf(os.Stderr, usage, stateDir())
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	o := serveFlags(fs)
	fs.Parse(args)

	if s := readState(); s != nil {
		fmt.Printf("already running (pid %d) at http://%s\n", s.PID, s.Addr)
		return nil
	}
	if healthy(o.addr) {
		return fmt.Errorf("%s is already serving something; stop it or pass -addr", o.addr)
	}
	if _, err := ghToken(); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()

	c := exec.Command(self, append([]string{"_daemon"}, args...)...)
	c.Stdout, c.Stderr = logf, logf
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive the terminal closing
	if err := c.Start(); err != nil {
		return err
	}
	pid := c.Process.Pid
	c.Process.Release()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if healthy(o.addr) {
			fmt.Printf("started (pid %d) at http://%s\n", pid, o.addr)
			return nil
		}
		if syscall.Kill(pid, 0) != nil {
			return fmt.Errorf("server exited during startup; see `github-overlay logs`")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("server did not become healthy within 10s; see `github-overlay logs`")
}

func cmdStop() error {
	s := readState()
	if s == nil {
		fmt.Println("not running")
		return nil
	}
	if err := syscall.Kill(s.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal pid %d: %w", s.PID, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(s.PID, 0) != nil {
			removeState()
			fmt.Printf("stopped (pid %d)\n", s.PID)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	syscall.Kill(s.PID, syscall.SIGKILL)
	removeState()
	fmt.Printf("killed (pid %d) after it ignored SIGTERM\n", s.PID)
	return nil
}

func cmdStatus() error {
	s := readState()
	if s == nil {
		fmt.Println("not running")
		return nil
	}
	health := "healthy"
	if !healthy(s.Addr) {
		health = "not responding"
	}
	fmt.Printf("running (pid %d) at http://%s — %s\nlogs: %s\n", s.PID, s.Addr, health, logPath())
	return nil
}

func cmdOpen(args []string) error {
	s := readState()
	if s == nil {
		if err := cmdStart(args); err != nil {
			return err
		}
		if s = readState(); s == nil {
			return errors.New("server started but its state file is missing")
		}
	}
	return exec.Command("open", "http://"+s.Addr).Run()
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow the log")
	fs.Parse(args)
	tailArgs := []string{"-n", "100"}
	if *follow {
		tailArgs = append(tailArgs, "-f")
	}
	c := exec.Command("tail", append(tailArgs, logPath())...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

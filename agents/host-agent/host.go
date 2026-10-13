package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ventrolab/ai-sre/pkg/agentkit"
)

type Host struct {
	// Glob patterns of readable log files.
	LogFiles []string
	// Named read-only commands (argv, no shell).
	Commands map[string][]string
	MaxSince time.Duration
}

var unitName = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]{1,200}$`)

// run executes an argv without a shell and returns combined output.
func run(ctx context.Context, argv ...string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"LANG=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0"}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil && out.Len() == 0 {
		return "", fmt.Errorf("%s: %w", argv[0], err)
	}
	return out.String(), nil
}

func grep(text, pattern string) (string, error) {
	if pattern == "" {
		return text, nil
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return "", fmt.Errorf("invalid grep: %w", err)
	}
	var kept []string
	for _, l := range strings.Split(text, "\n") {
		if re.MatchString(l) {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n"), nil
}

func (h *Host) Info(ctx context.Context, _ json.RawMessage) (string, error) {
	var b strings.Builder
	for _, argv := range [][]string{
		{"uname", "-a"}, {"uptime"}, {"free", "-m"},
		{"df", "-hT", "-x", "tmpfs", "-x", "devtmpfs", "-x", "overlay", "-x", "squashfs"},
		{"df", "-i", "-x", "tmpfs", "-x", "devtmpfs", "-x", "overlay", "-x", "squashfs"},
	} {
		out, err := run(ctx, argv...)
		if err != nil {
			out = err.Error()
		}
		fmt.Fprintf(&b, "$ %s\n%s\n", strings.Join(argv, " "), strings.TrimRight(out, "\n"))
	}
	return b.String(), nil
}

func (h *Host) Processes(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Sort  string `json:"sort"`
		Limit int    `json:"limit"`
	}
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	key := "-%cpu"
	if a.Sort == "mem" {
		key = "-%mem"
	}
	out, err := run(ctx, "ps", "-eo", "pid,user,%cpu,%mem,rss,etime,comm", "--sort="+key)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	n := agentkit.Clamp(a.Limit, 20, 100) + 1
	return strings.Join(lines[:min(n, len(lines))], "\n"), nil
}

func (h *Host) ServiceStatus(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Unit string `json:"unit"`
	}
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	if !unitName.MatchString(a.Unit) || strings.HasPrefix(a.Unit, "-") {
		return "", fmt.Errorf("invalid unit name %q", a.Unit)
	}
	return run(ctx, "systemctl", "status", "--no-pager", "--lines=20", "--", a.Unit)
}

var priorities = map[string]bool{"emerg": true, "alert": true, "crit": true, "err": true, "warning": true, "notice": true, "info": true, "debug": true}

func (h *Host) Journal(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Unit     string `json:"unit"`
		Since    string `json:"since"`
		Lines    int    `json:"lines"`
		Priority string `json:"priority"`
		Grep     string `json:"grep"`
	}
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	since, err := agentkit.Since(a.Since, time.Hour, h.MaxSince)
	if err != nil {
		return "", err
	}
	argv := []string{"journalctl", "--no-pager", "-o", "short-iso", "-n", fmt.Sprint(agentkit.Clamp(a.Lines, 200, 2000)),
		"--since", fmt.Sprintf("-%ds", int(since.Seconds()))}
	if a.Unit != "" {
		if !unitName.MatchString(a.Unit) || strings.HasPrefix(a.Unit, "-") {
			return "", fmt.Errorf("invalid unit name %q", a.Unit)
		}
		argv = append(argv, "-u", a.Unit)
	}
	if a.Priority != "" {
		if !priorities[a.Priority] {
			return "", fmt.Errorf("invalid priority %q", a.Priority)
		}
		argv = append(argv, "-p", a.Priority)
	}
	out, err := run(ctx, argv...)
	if err != nil {
		return "", err
	}
	return grep(out, a.Grep)
}

// allowed resolves symlinks and checks the real path against the globs.
func (h *Host) allowed(path string) (string, error) {
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("cannot read %s", path)
	}
	for _, g := range h.LogFiles {
		if ok, _ := filepath.Match(g, real); ok {
			return real, nil
		}
	}
	return "", fmt.Errorf("%s is not in the agent's log file allowlist", path)
}

func (h *Host) LogFile(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Path  string `json:"path"`
		Lines int    `json:"lines"`
		Grep  string `json:"grep"`
	}
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	path, err := h.allowed(a.Path)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// Read only the tail of large files.
	if st, err := f.Stat(); err == nil && st.Size() > 8<<20 {
		_, _ = f.Seek(st.Size()-8<<20, 0)
	}
	n := agentkit.Clamp(a.Lines, 200, 2000)
	var ring []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		ring = append(ring, sc.Text())
		if len(ring) > n*4 {
			ring = ring[len(ring)-n:]
		}
	}
	text, err := grep(strings.Join(ring, "\n"), a.Grep)
	if err != nil {
		return "", err
	}
	lines := strings.Split(text, "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n"), nil
}

func (h *Host) Command(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	if a.Name == "" {
		names := make([]string, 0, len(h.Commands))
		for n, argv := range h.Commands {
			names = append(names, fmt.Sprintf("- %s: %s", n, strings.Join(argv, " ")))
		}
		sort.Strings(names)
		if len(names) == 0 {
			return "No commands are configured.", nil
		}
		return strings.Join(names, "\n"), nil
	}
	argv, ok := h.Commands[a.Name]
	if !ok || len(argv) == 0 {
		return "", fmt.Errorf("unknown command %q; call host_command without a name to list them", a.Name)
	}
	return run(ctx, argv...)
}

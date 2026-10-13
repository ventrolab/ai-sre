package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ventrolab/ai-sre/pkg/agentkit"
)

// Write primitives for the Action Guard ("action:<primitive>").
type HostActions struct {
	// argv prefix for systemctl, e.g. [sudo, -n, systemctl] with a sudoers rule.
	Systemctl []string
	// Directories host.path.clean may clean (exact paths, no globs).
	CleanPaths []string
}

type hostActionArgs struct {
	Params map[string]any `json:"params"`
	DryRun bool           `json:"dry_run"`
}

func (h *HostActions) Toolset() map[string]agentkit.Tool {
	return map[string]agentkit.Tool{
		"action:host.systemd.restart": h.restart,
		"action:host.path.clean":      h.clean,
	}
}

func (h *HostActions) restart(ctx context.Context, raw json.RawMessage) (string, error) {
	var a hostActionArgs
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	unit, _ := a.Params["unit"].(string)
	if !unitName.MatchString(unit) || strings.HasPrefix(unit, "-") {
		return "", fmt.Errorf("invalid unit name %q", unit)
	}
	base := h.Systemctl
	if len(base) == 0 {
		base = []string{"systemctl"}
	}
	if a.DryRun {
		if _, err := run(ctx, append(append([]string{}, base...), "cat", "--", unit)...); err != nil {
			return "", err
		}
		return "dry run: would restart " + unit, nil
	}
	out, err := run(ctx, append(append([]string{}, base...), "restart", "--", unit)...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) != "" {
		return "", fmt.Errorf("restart %s: %s", unit, strings.TrimSpace(out))
	}
	return "restarted " + unit, nil
}

// clean removes regular files older than olderThan directly or below an
// allowlisted directory; symlinks are never followed.
func (h *HostActions) clean(_ context.Context, raw json.RawMessage) (string, error) {
	var a hostActionArgs
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	path, _ := a.Params["path"].(string)
	path = filepath.Clean(path)
	allowed := false
	for _, p := range h.CleanPaths {
		if filepath.Clean(p) == path {
			allowed = true
		}
	}
	if !allowed {
		return "", fmt.Errorf("%s is not in the agent's clean allowlist", path)
	}
	olderThan := 7 * 24 * time.Hour
	if s, _ := a.Params["olderThan"].(string); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < time.Hour {
			return "", fmt.Errorf("invalid olderThan %q (minimum 1h)", s)
		}
		olderThan = d
	}
	cutoff := time.Now().Add(-olderThan)
	var files int
	var bytes int64
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().After(cutoff) {
			return nil
		}
		if !a.DryRun {
			if os.Remove(p) != nil {
				return nil
			}
		}
		files++
		bytes += info.Size()
		return nil
	})
	if err != nil {
		return "", err
	}
	verb := "removed"
	if a.DryRun {
		verb = "dry run: would remove"
	}
	return fmt.Sprintf("%s %d files (%d MiB) older than %s under %s", verb, files, bytes>>20, olderThan, path), nil
}

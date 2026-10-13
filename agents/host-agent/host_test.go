package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogFileAllowlist(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "app.log")
	os.WriteFile(ok, []byte("a\nERROR one\nb\nerror two\n"), 0o644)
	secret := filepath.Join(dir, "shadow")
	os.WriteFile(secret, []byte("root:x"), 0o644)
	link := filepath.Join(dir, "evil.log")
	os.Symlink(secret, link)

	h := &Host{LogFiles: []string{filepath.Join(dir, "*.log")}, MaxSince: time.Hour}
	out, err := h.LogFile(context.Background(), json.RawMessage(`{"path":"`+ok+`","grep":"error"}`))
	if err != nil || out != "ERROR one\nerror two" {
		t.Fatalf("grep: %q %v", out, err)
	}
	for _, p := range []string{secret, link, dir + "/../" + filepath.Base(dir) + "/shadow"} {
		if _, err := h.LogFile(context.Background(), json.RawMessage(`{"path":"`+p+`"}`)); err == nil {
			t.Errorf("%s must be refused", p)
		}
	}
}

func TestUnitAndCommandValidation(t *testing.T) {
	h := &Host{Commands: map[string][]string{"echo": {"echo", "hello"}}, MaxSince: time.Hour}
	ctx := context.Background()
	for _, u := range []string{"--help", "a;rm -rf /", "x y"} {
		if _, err := h.ServiceStatus(ctx, json.RawMessage(`{"unit":"`+u+`"}`)); err == nil {
			t.Errorf("unit %q must be refused", u)
		}
	}
	if _, err := h.Journal(ctx, json.RawMessage(`{"priority":"all; reboot"}`)); err == nil {
		t.Error("bad priority must be refused")
	}
	if out, err := h.Command(ctx, json.RawMessage(`{"name":"echo"}`)); err != nil || strings.TrimSpace(out) != "hello" {
		t.Fatalf("command: %q %v", out, err)
	}
	if _, err := h.Command(ctx, json.RawMessage(`{"name":"rm"}`)); err == nil {
		t.Error("unknown command must be refused")
	}
}

func TestPathClean(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.tmp")
	os.WriteFile(old, []byte("x"), 0o644)
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(old, past, past)
	os.WriteFile(filepath.Join(dir, "new.tmp"), []byte("x"), 0o644)
	h := (&HostActions{CleanPaths: []string{dir}}).Toolset()["action:host.path.clean"]
	ctx := context.Background()
	if _, err := h(ctx, json.RawMessage(`{"params":{"path":"/etc"}}`)); err == nil {
		t.Fatal("path outside the allowlist must be refused")
	}
	out, err := h(ctx, json.RawMessage(`{"params":{"path":"`+dir+`","olderThan":"24h"},"dry_run":true}`))
	if err != nil || !strings.Contains(out, "would remove 1 files") {
		t.Fatalf("dry run: %q %v", out, err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("dry run removed a file")
	}
	if out, err = h(ctx, json.RawMessage(`{"params":{"path":"`+dir+`","olderThan":"24h"}}`)); err != nil || !strings.Contains(out, "removed 1 files") {
		t.Fatalf("clean: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.tmp")); err != nil {
		t.Fatal("new file must stay")
	}
}

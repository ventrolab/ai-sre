package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const incID = "6ae09a7d-8706-45c1-b344-987a8351e12c"

func fakeConsole(t *testing.T, unhealthy bool) *httptest.Server {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{
			"version": "0.2.1", "cluster": "prod-eu",
			"remediation": map[string]any{"mode": "readonly", "max_risk": "low", "max_actions_per_hour": 5,
				"playbooks": 8, "available": true},
			"llm":               map[string]any{"provider": "claude", "fallbacks": []string{"vllm"}},
			"incidents":         map[string]any{"open": 1, "by_status": map[string]int{"diagnosed": 1, "resolved": 4}, "opened_24h": 2, "opened_7d": 5, "mttd_seconds": 75.0, "mttr_seconds": nil, "top_alerts_7d": []map[string]any{{"alertname": "DiskSpaceLow", "n": 3}}},
			"pending_approvals": 0,
			"components":        []map[string]any{{"name": "Event Ingest", "ok": true, "detail": ""}, {"name": "HolmesGPT", "ok": !unhealthy, "detail": "HTTP 503"}},
		})
	})
	inc := map[string]any{"id": incID, "status": "diagnosed", "title": "DiskSpaceLow on db-07",
		"target": map[string]any{"cluster": "prod-eu", "host": "db-07"}, "confidence": 0.85,
		"created_at": "2026-10-12T12:35:05Z", "updated_at": "2026-10-12T20:21:05Z", "alert_count": 1}
	mux.HandleFunc("/api/v1/incidents", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "ffff" {
			write(w, map[string]any{"incidents": []any{}})
			return
		}
		write(w, map[string]any{"incidents": []any{inc}})
	})
	mux.HandleFunc("/api/v1/incidents/"+incID, func(w http.ResponseWriter, r *http.Request) {
		full := map[string]any{}
		for k, v := range inc {
			full[k] = v
		}
		full["root_cause"] = "Application logs in /var/log/app grew to 180 GiB"
		full["alerts"] = []map[string]any{{"alertname": "DiskSpaceLow", "status": "firing",
			"annotations": map[string]string{"summary": "Disk space is critically low"}}}
		full["events"] = []map[string]any{{"ts": "2026-10-12T12:36:00Z", "type": "diagnosed", "data": map[string]any{"confidence": 0.85}}}
		write(w, full)
	})
	mux.HandleFunc("/api/v1/agents", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		write(w, map[string]string{"error": "token lacks scope agents:read"})
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer aisre_test" {
			w.WriteHeader(http.StatusUnauthorized)
			write(w, map[string]string{"error": "invalid, expired or revoked API token"})
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runCLI(t *testing.T, srv *httptest.Server, args ...string) (int, string, string) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AISRE_URL", srv.URL+"/")
	t.Setenv("AISRE_TOKEN", "aisre_test")
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestStatus(t *testing.T) {
	srv := fakeConsole(t, false)
	code, out, errOut := runCLI(t, srv, "status")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"0.2.1, cluster prod-eu", "READONLY", "max risk low", "claude, fallbacks vllm",
		"1 open", "diagnosed 1", "1m15s / -", "ok  Event Ingest", "DiskSpaceLow"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "resolved 4") {
		t.Errorf("resolved incidents are not open:\n%s", out)
	}

	code, out, _ = runCLI(t, fakeConsole(t, true), "status", "-o", "json")
	if code != 2 {
		t.Fatalf("an unhealthy component must exit 2, got %d", code)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil || v["cluster"] != "prod-eu" {
		t.Fatalf("json output: %v %s", err, out)
	}
}

func TestIncidents(t *testing.T) {
	srv := fakeConsole(t, false)
	code, out, errOut := runCLI(t, srv, "incidents")
	if code != 0 || !strings.Contains(out, "6ae09a7d") || !strings.Contains(out, "prod-eu host/db-07") &&
		!strings.Contains(out, "host/db-07") || !strings.Contains(out, "85%") {
		t.Fatalf("exit %d %s\n%s", code, errOut, out)
	}
	// A prefix from the list resolves to the incident; flags may follow the id.
	code, out, errOut = runCLI(t, srv, "incident", "6ae09a7d", "-o", "table")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"DiskSpaceLow on db-07", "Application logs in /var/log/app", "firing",
		"Timeline", "confidence=0.85", srv.URL + "/incidents/" + incID} {
		if !strings.Contains(out, want) {
			t.Errorf("incident output lacks %q:\n%s", want, out)
		}
	}
	if code, _, errOut = runCLI(t, srv, "incident", "ffff"); code == 0 || !strings.Contains(errOut, "no incident") {
		t.Fatalf("unknown prefix: %d %s", code, errOut)
	}
}

func TestErrors(t *testing.T) {
	srv := fakeConsole(t, false)
	code, _, errOut := runCLI(t, srv, "agents")
	if code != 1 || !strings.Contains(errOut, "agents:read") {
		t.Fatalf("scope error: %d %s", code, errOut)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AISRE_URL", "")
	t.Setenv("AISRE_TOKEN", "")
	var out, errBuf bytes.Buffer
	if code := run([]string{"status"}, &out, &errBuf); code != 1 || !strings.Contains(errBuf.String(), "aisre login") {
		t.Fatalf("missing config: %d %s", code, errBuf.String())
	}
	if code := run([]string{"bogus"}, &out, &errBuf); code != 1 {
		t.Fatalf("unknown command: %d", code)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := saveConfig(Config{URL: "https://c.example", Token: "aisre_x"}); err != nil {
		t.Fatal(err)
	}
	if c := loadConfig(); c.URL != "https://c.example" || c.Token != "aisre_x" {
		t.Fatalf("config: %+v", c)
	}
}

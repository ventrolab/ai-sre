package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBuildQueries(t *testing.T) {
	q := LogQuery{Namespace: "demo", Pod: "crashy", Contains: "FATAL", Regex: "config.*missing"}
	cases := map[string][]string{
		"loki":          {`namespace="demo"`, `pod=~"crashy.*"`, `|= "FATAL"`, `|~ "(?i)config.*missing"`},
		"victorialogs":  {`_time:1h0m0s`, `"kubernetes.pod_namespace":="demo"`, `"kubernetes.pod_name":"crashy"*`, `~"FATAL"`},
		"elasticsearch": {`kubernetes.namespace_name:"demo"`, `kubernetes.pod_name:crashy*`, `message:"FATAL"`, `message:/config.*missing/`},
	}
	for typ, want := range cases {
		got, err := buildQuery(&LogBackend{Type: typ}, q, time.Hour)
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s query %q lacks %q", typ, got, w)
			}
		}
	}
	if _, err := buildQuery(&LogBackend{Type: "loki"}, LogQuery{Contains: "x"}, time.Hour); err == nil {
		t.Error("loki without a stream selector must be rejected")
	}
}

func TestBackendsParseResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/loki/"):
			w.Write([]byte(`{"data":{"result":[{"stream":{"namespace":"demo","pod":"crashy-1"},"values":[["1760000000000000000","FATAL boom"]]}]}}`))
		case r.URL.Path == "/select/logsql/query":
			if !strings.Contains(r.FormValue("query"), "sort by (_time desc)") {
				t.Error("VictoriaLogs query must sort by time")
			}
			w.Write([]byte(`{"_time":"2026-10-11T10:00:00Z","_msg":"FATAL vl","kubernetes.pod_namespace":"demo","kubernetes.pod_name":"crashy-1"}` + "\n"))
		case strings.HasSuffix(r.URL.Path, "/_search"):
			w.Write([]byte(`{"hits":{"hits":[{"_source":{"@timestamp":"2026-10-11T10:00:00Z","message":"FATAL es","kubernetes":{"namespace_name":"demo","pod_name":"crashy-1"}}}]}}`))
		}
	}))
	defer srv.Close()
	for _, typ := range []string{"loki", "victorialogs", "elasticsearch"} {
		l := &Logs{backends: []LogBackend{{Name: "b", Type: typ, URL: srv.URL}}, http: srv.Client(), maxSince: 24 * time.Hour}
		out, err := l.Query(context.Background(), json.RawMessage(`{"namespace":"demo","pod":"crashy"}`))
		if err != nil || !strings.Contains(out, "[demo/crashy-1] FATAL") {
			t.Errorf("%s: %q %v", typ, out, err)
		}
	}
	l := &Logs{backends: []LogBackend{{Name: "b", Type: "loki", URL: srv.URL}}, http: srv.Client(), maxSince: time.Hour}
	if _, err := l.Query(context.Background(), json.RawMessage(`{"namespace":"demo","since":"48h"}`)); err == nil {
		t.Error("window above maxLogWindow must be rejected")
	}
}

func TestRenderTable(t *testing.T) {
	body := `{"kind":"Table","apiVersion":"meta.k8s.io/v1","columnDefinitions":[{"name":"Name","priority":0},{"name":"Ready","priority":0},{"name":"IP","priority":1}],
	"rows":[{"cells":["web-1","1/1","10.0.0.1"],"object":{"metadata":{"name":"web-1","namespace":"shop"}}}]}`
	out, err := renderTable([]byte(body), true)
	if err != nil || !strings.Contains(out, "NAMESPACE") || !strings.Contains(out, "shop") || strings.Contains(out, "10.0.0.1") {
		t.Fatalf("table: %q %v", out, err)
	}
}

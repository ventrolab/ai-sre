package guard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeHub struct {
	calls    []string
	failOn   string
	verifyOK bool
}

func (f *fakeHub) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Error("missing action token")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := r.URL.Path
		if p, ok := body["primitive"].(string); ok {
			name = p
			if body["dry_run"] == true {
				name = "dry:" + p
			}
			if body["check"] == true {
				name = "check:" + p
			}
		}
		f.calls = append(f.calls, name)
		switch {
		case name == f.failOn:
			json.NewEncoder(w).Encode(hubResult{Error: "boom"})
		case r.URL.Path == "/v1/query" && !f.verifyOK:
			json.NewEncoder(w).Encode(hubResult{OK: true, Output: "Empty result: no series match."})
		default:
			json.NewEncoder(w).Encode(hubResult{OK: true, Output: "done"})
		}
	}))
}

func evaluation() EvalResponse {
	return EvalResponse{
		Action:   &RenderedStep{Primitive: "k8s.deployment.scale", Params: map[string]any{"delta": 1}},
		Precheck: []RenderedStep{{Primitive: "k8s.deployment.noActiveHPA"}},
		Rollback: &RenderedStep{Primitive: "k8s.deployment.scale", Params: map[string]any{"delta": -1}},
		Verify:   &Verify{PromQL: "up == 1", Timeout: "1s"},
	}
}

func TestExecutorOutcomes(t *testing.T) {
	cases := map[string]struct {
		hub    fakeHub
		status string
		calls  string
	}{
		"verified":        {fakeHub{verifyOK: true}, "succeeded", "check:k8s.deployment.noActiveHPA,dry:k8s.deployment.scale,k8s.deployment.scale,/v1/query"},
		"precheck blocks": {fakeHub{failOn: "check:k8s.deployment.noActiveHPA"}, "refused", "check:k8s.deployment.noActiveHPA"},
		"dry run blocks":  {fakeHub{failOn: "dry:k8s.deployment.scale"}, "refused", "check:k8s.deployment.noActiveHPA,dry:k8s.deployment.scale"},
		"verify fails":    {fakeHub{}, "rolled_back", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			hub := c.hub
			srv := hub.server(t)
			defer srv.Close()
			e := &Executor{Hub: &HubClient{URL: srv.URL, Token: "tok", HTTP: srv.Client()}, PollEvery: 300 * time.Millisecond, MaxTimeout: time.Minute}
			res := e.Run(context.Background(), "i1", evaluation(), "edge")
			if res.Status != c.status {
				t.Fatalf("status %s (%s), steps %+v", res.Status, res.Reason, res.Steps)
			}
			if c.calls != "" && strings.Join(hub.calls, ",") != c.calls {
				t.Fatalf("calls %v", hub.calls)
			}
			if name == "verify fails" && hub.calls[len(hub.calls)-1] != "k8s.deployment.scale" {
				t.Fatalf("rollback not executed: %v", hub.calls)
			}
		})
	}
}

func TestTargetName(t *testing.T) {
	if TargetName(Target{Kind: "host.unit", Host: "db1", Cluster: "x"}) != "db1" || TargetName(Target{Kind: "k8s.pod", Cluster: "edge"}) != "edge" {
		t.Fatal("wrong target")
	}
}

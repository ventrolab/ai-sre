package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HubClient calls the Tool Hub write path (actions) and read path (verify).
type HubClient struct {
	URL   string
	Token string
	HTTP  *http.Client
}

type hubResult struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
	Error  string `json:"error"`
}

func (c *HubClient) post(ctx context.Context, path string, body any) hubResult {
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return hubResult{Error: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return hubResult{Error: "tool hub: " + err.Error()}
	}
	defer resp.Body.Close()
	var r hubResult
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return hubResult{Error: fmt.Sprintf("tool hub returned %s", resp.Status)}
	}
	return r
}

func (c *HubClient) Action(ctx context.Context, incident, target string, step RenderedStep, dryRun, check bool) hubResult {
	return c.post(ctx, "/v1/actions", map[string]any{
		"incident_id": incident, "target": target, "primitive": step.Primitive,
		"params": step.Params, "dry_run": dryRun, "check": check,
	})
}

func (c *HubClient) PromQL(ctx context.Context, target, query string) hubResult {
	return c.post(ctx, "/v1/query", map[string]any{
		"target": target, "tool": "prometheus_query", "args": map[string]any{"query": query},
	})
}

type ExecStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Output string `json:"output"`
}

type ExecResult struct {
	// succeeded | failed | rolled_back | rollback_failed | refused
	Status     string       `json:"status"`
	Reason     string       `json:"reason"`
	Target     string       `json:"target"`
	Steps      []ExecStep   `json:"steps"`
	Evaluation EvalResponse `json:"evaluation"`
}

// TargetName is the hub target of an incident: the host for host targets,
// otherwise the cluster.
func TargetName(t Target) string {
	if strings.HasPrefix(t.Kind, "host") && t.Host != "" {
		return t.Host
	}
	return t.Cluster
}

type Executor struct {
	Hub        *HubClient
	PollEvery  time.Duration
	MaxTimeout time.Duration
}

// Run: prechecks -> dry run -> action -> verify -> rollback on failed verify.
func (e *Executor) Run(ctx context.Context, incident string, ev EvalResponse, target string) ExecResult {
	res := ExecResult{Target: target, Evaluation: ev}
	step := func(name string, r hubResult) bool {
		out := r.Output
		if !r.OK {
			out = r.Error
		}
		res.Steps = append(res.Steps, ExecStep{Name: name, OK: r.OK, Output: out})
		return r.OK
	}
	finish := func(status, reason string) ExecResult {
		res.Status, res.Reason = status, reason
		return res
	}
	if target == "" {
		return finish("refused", "the incident has no cluster or host to act on")
	}
	for _, c := range ev.Precheck {
		if !step("precheck "+c.Primitive, e.Hub.Action(ctx, incident, target, c, false, true)) {
			return finish("refused", "precheck failed")
		}
	}
	if !step("dry run "+ev.Action.Primitive, e.Hub.Action(ctx, incident, target, *ev.Action, true, false)) {
		return finish("refused", "dry run failed")
	}
	if !step("execute "+ev.Action.Primitive, e.Hub.Action(ctx, incident, target, *ev.Action, false, false)) {
		return finish("failed", "action failed")
	}
	if ev.Verify == nil {
		return finish("succeeded", "executed; the playbook has no verification")
	}
	if ok, detail := e.verify(ctx, target, *ev.Verify); ok {
		step("verify", hubResult{OK: true, Output: detail})
		return finish("succeeded", "verified")
	} else {
		step("verify", hubResult{Error: detail})
	}
	if ev.Rollback == nil {
		return finish("failed", "verification failed; the playbook has no rollback")
	}
	if step("rollback "+ev.Rollback.Primitive, e.Hub.Action(ctx, incident, target, *ev.Rollback, false, false)) {
		return finish("rolled_back", "verification failed; rolled back")
	}
	return finish("rollback_failed", "verification failed and the rollback failed: needs a human")
}

// verify polls the PromQL condition until it returns a series or times out.
func (e *Executor) verify(ctx context.Context, target string, v Verify) (bool, string) {
	timeout, err := time.ParseDuration(v.Timeout)
	if err != nil || timeout <= 0 {
		timeout = 5 * time.Minute
	}
	timeout = min(timeout, e.MaxTimeout)
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		r := e.Hub.PromQL(ctx, target, v.PromQL)
		switch {
		case r.OK && !strings.HasPrefix(r.Output, "Empty result"):
			return true, "condition holds: " + strings.TrimSpace(r.Output)
		case r.OK:
			last = "condition not met yet"
		default:
			last = r.Error
		}
		if time.Now().Add(e.PollEvery).After(deadline) {
			return false, fmt.Sprintf("not verified within %s: %s", timeout, last)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err().Error()
		case <-time.After(e.PollEvery):
		}
	}
}

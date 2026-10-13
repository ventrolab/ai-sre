package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/ventrolab/ai-sre/internal/redact"
	"github.com/ventrolab/ai-sre/pkg/tools"
	"github.com/ventrolab/ai-sre/pkg/tunnel"
)

var (
	callsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aisre_toolhub_calls_total", Help: "Tool calls routed to agents",
	}, []string{"tool", "target", "result"})
	callSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "aisre_toolhub_call_seconds", Help: "Tool call duration",
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"tool"})
	agentsConnected = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "aisre_toolhub_agents_connected", Help: "Connected agents",
	})
)

type Hub struct {
	reg       *tunnel.Registry
	log       *slog.Logger
	timeout   time.Duration
	maxOutput int
}

// MCPServer exposes the whole catalog at once, so the tool list does not
// depend on which agents happen to be connected when HolmesGPT starts.
func (h *Hub) MCPServer(version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "ai-sre-tool-hub", Version: version}, nil)
	s.AddTool(&mcp.Tool{
		Name: "list_targets",
		Description: "List the clusters and hosts the AI SRE agents can reach, with labels and tools. " +
			"Call this first to find the target name for the other tools.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, h.listTargets)
	for _, spec := range tools.Catalog {
		spec := spec
		s.AddTool(&mcp.Tool{Name: spec.Name, Description: spec.Description, InputSchema: spec.Schema()},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return h.call(ctx, spec, req.Params.Arguments), nil
			})
	}
	return s
}

func text(s string, isErr bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}, IsError: isErr}
}

func (h *Hub) listTargets(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	agents := h.reg.List()
	if len(agents) == 0 {
		return text("No agents are connected.", false), nil
	}
	var b strings.Builder
	for _, a := range agents {
		labels := make([]string, 0, len(a.Labels))
		for k, v := range a.Labels {
			labels = append(labels, k+"="+v)
		}
		sort.Strings(labels)
		fmt.Fprintf(&b, "- %s (%s) labels: %s\n  tools: %s\n", a.Name, a.Kind,
			strings.Join(labels, ", "), strings.Join(a.Tools, ", "))
	}
	return text(b.String(), false), nil
}

// resolve picks the agent: the named target, or the only agent serving the tool.
func (h *Hub) resolve(target, tool string) (*tunnel.Agent, error) {
	if target != "" {
		a := h.reg.Get(target)
		if a == nil {
			return nil, fmt.Errorf("target %q is not connected; call list_targets", target)
		}
		if !a.HasTool(tool) {
			return nil, fmt.Errorf("target %q (%s) does not serve %s", target, a.Kind, tool)
		}
		return a, nil
	}
	var found []*tunnel.Agent
	for _, a := range h.reg.List() {
		if a.HasTool(tool) {
			found = append(found, a)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("no connected target serves %s", tool)
	case 1:
		return found[0], nil
	}
	names := make([]string, len(found))
	for i, a := range found {
		names[i] = a.Name
	}
	return nil, fmt.Errorf("several targets serve %s (%s); pass target", tool, strings.Join(names, ", "))
}

func (h *Hub) call(ctx context.Context, spec tools.Spec, raw json.RawMessage) *mcp.CallToolResult {
	args := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return text("invalid arguments: "+err.Error(), true)
		}
	}
	target, _ := args["target"].(string)
	delete(args, "target")
	agent, err := h.resolve(target, spec.Name)
	if err != nil {
		callsTotal.WithLabelValues(spec.Name, target, "unrouted").Inc()
		return text(err.Error(), true)
	}
	payload, _ := json.Marshal(args)
	started := time.Now()
	out, err := agent.Call(ctx, spec.Name, payload, h.timeout)
	callSeconds.WithLabelValues(spec.Name).Observe(time.Since(started).Seconds())
	result := "ok"
	if err != nil {
		result = "error"
	}
	callsTotal.WithLabelValues(spec.Name, agent.Name, result).Inc()
	h.log.Info("tool call", "tool", spec.Name, "target", agent.Name, "args", string(payload),
		"result", result, "ms", time.Since(started).Milliseconds(), "bytes", len(out))
	if err != nil {
		return text(fmt.Sprintf("%s on %s failed: %s", spec.Name, agent.Name, redact.Secrets(err.Error())), true)
	}
	if out == "" {
		out = "(no output)"
	}
	return text(redact.Truncate(redact.Secrets(out), h.maxOutput), false)
}

// ActionRequest is sent by the Action Guard only (separate token).
type ActionRequest struct {
	Target    string         `json:"target"`
	Primitive string         `json:"primitive"`
	Params    map[string]any `json:"params"`
	DryRun    bool           `json:"dry_run"`
	Check     bool           `json:"check"`
	Incident  string         `json:"incident_id"`
}

type QueryRequest struct {
	Target string         `json:"target"`
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args"`
}

type callResult struct {
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Action runs a write primitive or a precheck on the target's agent.
func (h *Hub) Action(ctx context.Context, r ActionRequest) callResult {
	tool := "action:" + r.Primitive
	if r.Check {
		tool = "check:" + r.Primitive
	}
	agent := h.reg.Get(r.Target)
	if agent == nil {
		return callResult{Error: fmt.Sprintf("target %q is not connected", r.Target)}
	}
	if !agent.HasTool(tool) {
		return callResult{Error: fmt.Sprintf("target %q does not allow %s (write disabled or unsupported)", r.Target, tool)}
	}
	payload, _ := json.Marshal(map[string]any{"params": r.Params, "dry_run": r.DryRun})
	started := time.Now()
	out, err := agent.Call(ctx, tool, payload, h.timeout)
	res := "ok"
	if err != nil {
		res = "error"
	}
	callsTotal.WithLabelValues(tool, agent.Name, res).Inc()
	h.log.Info("action", "incident", r.Incident, "target", agent.Name, "tool", tool, "params", string(payload),
		"result", res, "ms", time.Since(started).Milliseconds(), "output", out, "error", fmt.Sprint(err))
	if err != nil {
		return callResult{Error: redact.Secrets(err.Error())}
	}
	return callResult{OK: true, Output: redact.Truncate(redact.Secrets(out), h.maxOutput)}
}

// Query runs a read tool for the guard's verification step.
func (h *Hub) Query(ctx context.Context, r QueryRequest) callResult {
	spec, ok := tools.ByName(r.Tool)
	if !ok {
		return callResult{Error: "unknown tool " + r.Tool}
	}
	args := map[string]any{"target": r.Target}
	for k, v := range r.Args {
		args[k] = v
	}
	raw, _ := json.Marshal(args)
	res := h.call(ctx, spec, raw)
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError {
		return callResult{Error: text}
	}
	return callResult{OK: true, Output: text}
}

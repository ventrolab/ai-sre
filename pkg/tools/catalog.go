// Package tools is the catalog of read-only tools agents can serve. The hub
// exposes every entry as an MCP tool with an extra "target" argument.
package tools

type Spec struct {
	Name        string
	Kind        string // cluster | host
	Description string
	// JSON Schema properties and required names, without "target".
	Properties map[string]any
	Required   []string
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

const sinceDesc = "Time window back from now, e.g. 15m, 1h, 6h (default 1h, max 24h)."

var Catalog = []Spec{
	// Kubernetes (k8s-agent)
	{Name: "k8s_list", Kind: "cluster",
		Description: "List Kubernetes objects of a kind as a table (like kubectl get). Secrets are not available.",
		Properties: map[string]any{
			"kind":           str("Resource kind or plural, e.g. pods, deployments, nodes, events, ingresses.networking.k8s.io"),
			"namespace":      str("Namespace; empty for all namespaces or cluster-scoped kinds"),
			"label_selector": str("Label selector, e.g. app=web"),
			"field_selector": str("Field selector, e.g. status.phase!=Running"),
			"limit":          integer("Max rows (default 200, max 1000)"),
		}, Required: []string{"kind"}},
	{Name: "k8s_get", Kind: "cluster",
		Description: "Get one Kubernetes object as YAML (managedFields removed). Secrets are not available.",
		Properties: map[string]any{
			"kind":      str("Resource kind or plural"),
			"namespace": str("Namespace (empty for cluster-scoped kinds)"),
			"name":      str("Object name"),
		}, Required: []string{"kind", "name"}},
	{Name: "k8s_events", Kind: "cluster",
		Description: "Kubernetes events, newest last; optionally only for one object.",
		Properties: map[string]any{
			"namespace": str("Namespace; empty for all"),
			"object":    str("Name of the involved object, e.g. a pod name"),
			"limit":     integer("Max events (default 100)"),
		}},
	{Name: "k8s_logs", Kind: "cluster",
		Description: "Container logs of a pod, without filtering by default. Use previous=true for the crashed instance.",
		Properties: map[string]any{
			"namespace": str("Namespace"),
			"pod":       str("Pod name"),
			"container": str("Container name (default: the only or first container)"),
			"previous":  boolean("Logs of the previous, terminated container instance"),
			"tail":      integer("Last N lines (default 200, max 2000)"),
			"since":     str("Only lines newer than this, e.g. 30m"),
			"grep":      str("Case-insensitive regular expression to keep matching lines only"),
		}, Required: []string{"namespace", "pod"}},
	{Name: "prometheus_query", Kind: "cluster",
		Description: "Instant PromQL query against the target's Prometheus-compatible metrics backend.",
		Properties: map[string]any{
			"query": str("PromQL expression"),
			"time":  str("RFC3339 time or unix seconds (default now)"),
		}, Required: []string{"query"}},
	{Name: "prometheus_range_query", Kind: "cluster",
		Description: "Range PromQL query; returns a compact series summary (min, max, last, points).",
		Properties: map[string]any{
			"query": str("PromQL expression"),
			"since": str(sinceDesc),
			"step":  str("Resolution step, e.g. 30s, 5m (default: about 60 points)"),
		}, Required: []string{"query"}},
	{Name: "logs_backends", Kind: "cluster",
		Description: "Log backends configured for the target (name, type) and the selector fields they map."},
	{Name: "logs_query", Kind: "cluster",
		Description: "Search centralized logs with a backend-neutral selector (Loki, VictoriaLogs, Elasticsearch, OpenSearch).",
		Properties: map[string]any{
			"backend":   str("Backend name (default: the first configured)"),
			"namespace": str("Kubernetes namespace"),
			"pod":       str("Pod name or prefix"),
			"container": str("Container name"),
			"host":      str("Host name"),
			"unit":      str("Systemd unit"),
			"contains":  str("Substring the line must contain"),
			"regex":     str("Regular expression the line must match"),
			"since":     str(sinceDesc),
			"limit":     integer("Max lines (default 200, max 1000)"),
		}},
	{Name: "logs_native", Kind: "cluster",
		Description: "Run a native log query (LogQL, LogsQL, or Elasticsearch query_string) when logs_query is not expressive enough.",
		Properties: map[string]any{
			"backend": str("Backend name (default: the first configured)"),
			"query":   str("Query in the backend's language"),
			"since":   str(sinceDesc),
			"limit":   integer("Max lines (default 200, max 1000)"),
		}, Required: []string{"query"}},

	// Hosts (host-agent)
	{Name: "host_info", Kind: "host",
		Description: "Host overview: kernel, uptime and load, memory, filesystems."},
	{Name: "host_processes", Kind: "host",
		Description: "Top processes by CPU or memory.",
		Properties: map[string]any{
			"sort":  str("cpu (default) or mem"),
			"limit": integer("Max processes (default 20, max 100)"),
		}},
	{Name: "host_service_status", Kind: "host",
		Description: "systemctl status of a unit.",
		Properties:  map[string]any{"unit": str("Unit name, e.g. nginx.service")}, Required: []string{"unit"}},
	{Name: "host_journal", Kind: "host",
		Description: "systemd journal lines, newest last, without a filter by default.",
		Properties: map[string]any{
			"unit":     str("Only this unit"),
			"since":    str(sinceDesc),
			"lines":    integer("Max lines (default 200, max 2000)"),
			"priority": str("Max priority: emerg, alert, crit, err, warning, notice, info, debug"),
			"grep":     str("Case-insensitive regular expression"),
		}},
	{Name: "host_log_file", Kind: "host",
		Description: "Tail of an allowlisted log file on the host.",
		Properties: map[string]any{
			"path":  str("File path; must match the agent's allowlist"),
			"lines": integer("Last N lines (default 200, max 2000)"),
			"grep":  str("Case-insensitive regular expression"),
		}, Required: []string{"path"}},
	{Name: "host_command", Kind: "host",
		Description: "Run one of the agent's predefined read-only commands by name; call without a name to list them.",
		Properties:  map[string]any{"name": str("Command name from the agent's allowlist")}},
}

func ByName(name string) (Spec, bool) {
	for _, s := range Catalog {
		if s.Name == name {
			return s, true
		}
	}
	return Spec{}, false
}

// Schema returns the MCP input schema, with the routing argument "target".
func (s Spec) Schema() map[string]any {
	props := map[string]any{
		"target": str("Target name from list_targets: a cluster or a host. May be omitted when only one connected target serves this tool."),
	}
	for k, v := range s.Properties {
		props[k] = v
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(s.Required) > 0 {
		schema["required"] = s.Required
	}
	return schema
}

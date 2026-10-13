package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ventrolab/ai-sre/pkg/agentkit"
)

// LogBackend config: one Loki, VictoriaLogs or Elasticsearch/OpenSearch endpoint.
type LogBackend struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"` // loki | victorialogs | elasticsearch
	Flavor string            `json:"flavor,omitempty"`
	URL    string            `json:"url"`
	Index  string            `json:"index,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
	Auth   struct {
		BearerTokenEnv string `json:"bearerTokenEnv,omitempty"`
		UsernameEnv    string `json:"usernameEnv,omitempty"`
		PasswordEnv    string `json:"passwordEnv,omitempty"`
	} `json:"auth"`
}

// Selector fields of logs_query, mapped to backend field or label names.
var selectorFields = []string{"namespace", "pod", "container", "host", "unit"}

var defaultFields = map[string]map[string]string{
	"loki": {"namespace": "namespace", "pod": "pod", "container": "container", "host": "host", "unit": "unit"},
	"victorialogs": {"namespace": "kubernetes.pod_namespace", "pod": "kubernetes.pod_name",
		"container": "kubernetes.container_name", "host": "kubernetes.pod_node_name", "unit": "_SYSTEMD_UNIT"},
	"elasticsearch": {"namespace": "kubernetes.namespace_name", "pod": "kubernetes.pod_name",
		"container": "kubernetes.container_name", "host": "host.hostname", "unit": "systemd.unit",
		"message": "message", "timestamp": "@timestamp"},
}

func (b *LogBackend) field(name string) string {
	if v := b.Fields[name]; v != "" {
		return v
	}
	return defaultFields[b.Type][name]
}

type LogLine struct {
	Time   time.Time
	Source string
	Msg    string
}

type LogQuery struct {
	Backend                               string `json:"backend"`
	Namespace, Pod, Container, Host, Unit string
	Contains                              string `json:"contains"`
	Regex                                 string `json:"regex"`
	Since                                 string `json:"since"`
	Limit                                 int    `json:"limit"`
}

type Logs struct {
	backends []LogBackend
	http     *http.Client
	maxSince time.Duration
}

func (l *Logs) pick(name string) (*LogBackend, error) {
	if len(l.backends) == 0 {
		return nil, fmt.Errorf("no log backends are configured on this agent")
	}
	if name == "" {
		return &l.backends[0], nil
	}
	for i := range l.backends {
		if l.backends[i].Name == name {
			return &l.backends[i], nil
		}
	}
	return nil, fmt.Errorf("unknown log backend %q", name)
}

func (l *Logs) Backends(context.Context, json.RawMessage) (string, error) {
	if len(l.backends) == 0 {
		return "No log backends are configured.", nil
	}
	var b strings.Builder
	for _, be := range l.backends {
		fmt.Fprintf(&b, "- %s (%s%s)", be.Name, be.Type, map[bool]string{true: "/" + be.Flavor, false: ""}[be.Flavor != ""])
		for _, f := range selectorFields {
			fmt.Fprintf(&b, " %s=%s", f, be.field(f))
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func (l *Logs) Query(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		LogQuery
		Namespace string `json:"namespace"`
		Pod       string `json:"pod"`
		Container string `json:"container"`
		Host      string `json:"host"`
		Unit      string `json:"unit"`
	}
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	q := a.LogQuery
	q.Namespace, q.Pod, q.Container, q.Host, q.Unit = a.Namespace, a.Pod, a.Container, a.Host, a.Unit
	if q.Regex != "" {
		if _, err := regexp.Compile(q.Regex); err != nil {
			return "", fmt.Errorf("invalid regex: %w", err)
		}
	}
	be, err := l.pick(q.Backend)
	if err != nil {
		return "", err
	}
	since, err := agentkit.Since(q.Since, time.Hour, l.maxSince)
	if err != nil {
		return "", err
	}
	limit := agentkit.Clamp(q.Limit, 200, 1000)
	native, err := buildQuery(be, q, since)
	if err != nil {
		return "", err
	}
	lines, err := l.run(ctx, be, native, since, limit)
	if err != nil {
		return "", err
	}
	return format(lines, "query: "+native), nil
}

func (l *Logs) Native(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Backend string `json:"backend"`
		Query   string `json:"query"`
		Since   string `json:"since"`
		Limit   int    `json:"limit"`
	}
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	be, err := l.pick(a.Backend)
	if err != nil {
		return "", err
	}
	since, err := agentkit.Since(a.Since, time.Hour, l.maxSince)
	if err != nil {
		return "", err
	}
	query := a.Query
	if be.Type == "victorialogs" && !strings.Contains(query, "_time:") {
		query = fmt.Sprintf("_time:%s %s", since, query)
	}
	lines, err := l.run(ctx, be, query, since, agentkit.Clamp(a.Limit, 200, 1000))
	if err != nil {
		return "", err
	}
	return format(lines, "query: "+query), nil
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func selector(q LogQuery) map[string]string {
	return map[string]string{"namespace": q.Namespace, "pod": q.Pod, "container": q.Container, "host": q.Host, "unit": q.Unit}
}

// buildQuery turns the neutral selector into the backend's query language.
func buildQuery(be *LogBackend, q LogQuery, since time.Duration) (string, error) {
	sel := selector(q)
	switch be.Type {
	case "loki":
		var m []string
		for _, f := range selectorFields {
			if v := sel[f]; v != "" {
				if f == "pod" {
					m = append(m, fmt.Sprintf("%s=~%s", be.field(f), quote(regexp.QuoteMeta(v)+".*")))
				} else {
					m = append(m, fmt.Sprintf("%s=%s", be.field(f), quote(v)))
				}
			}
		}
		if len(m) == 0 {
			return "", fmt.Errorf("Loki needs at least one of namespace, pod, container, host, unit")
		}
		s := "{" + strings.Join(m, ", ") + "}"
		if q.Contains != "" {
			s += " |= " + quote(q.Contains)
		}
		if q.Regex != "" {
			s += " |~ " + quote("(?i)"+q.Regex)
		}
		return s, nil
	case "victorialogs":
		parts := []string{fmt.Sprintf("_time:%s", since)}
		for _, f := range selectorFields {
			if v := sel[f]; v != "" {
				if f == "pod" {
					parts = append(parts, fmt.Sprintf("%s:%s*", quote(be.field(f)), quote(v)))
				} else {
					parts = append(parts, fmt.Sprintf("%s:=%s", quote(be.field(f)), quote(v)))
				}
			}
		}
		if q.Contains != "" {
			parts = append(parts, "~"+quote(regexp.QuoteMeta(q.Contains)))
		}
		if q.Regex != "" {
			parts = append(parts, "~"+quote("(?i)"+q.Regex))
		}
		return strings.Join(parts, " "), nil
	case "elasticsearch":
		var parts []string
		for _, f := range selectorFields {
			if v := sel[f]; v != "" {
				esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v)
				if f == "pod" {
					parts = append(parts, fmt.Sprintf(`%s:%s*`, be.field(f), strings.ReplaceAll(esc, " ", `\ `)))
				} else {
					parts = append(parts, fmt.Sprintf(`%s:"%s"`, be.field(f), esc))
				}
			}
		}
		if q.Contains != "" {
			parts = append(parts, fmt.Sprintf(`%s:%s`, be.field("message"), quote(q.Contains)))
		}
		if q.Regex != "" {
			parts = append(parts, fmt.Sprintf(`%s:/%s/`, be.field("message"), strings.ReplaceAll(q.Regex, "/", `\/`)))
		}
		if len(parts) == 0 {
			return "*", nil
		}
		return strings.Join(parts, " AND "), nil
	}
	return "", fmt.Errorf("unsupported backend type %q", be.Type)
}

func (l *Logs) request(ctx context.Context, be *LogBackend, method, u string, body io.Reader, ctype string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if be.Auth.BearerTokenEnv != "" {
		req.Header.Set("Authorization", "Bearer "+os.Getenv(be.Auth.BearerTokenEnv))
	} else if be.Auth.UsernameEnv != "" {
		req.SetBasicAuth(os.Getenv(be.Auth.UsernameEnv), os.Getenv(be.Auth.PasswordEnv))
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s returned %s: %s", be.Name, resp.Status, strings.TrimSpace(string(data[:min(len(data), 300)])))
	}
	return data, nil
}

func (l *Logs) run(ctx context.Context, be *LogBackend, query string, since time.Duration, limit int) ([]LogLine, error) {
	base := strings.TrimRight(be.URL, "/")
	now := time.Now()
	var lines []LogLine
	switch be.Type {
	case "loki":
		q := url.Values{"query": {query}, "limit": {strconv.Itoa(limit)}, "direction": {"backward"},
			"start": {strconv.FormatInt(now.Add(-since).UnixNano(), 10)}, "end": {strconv.FormatInt(now.UnixNano(), 10)}}
		data, err := l.request(ctx, be, http.MethodGet, base+"/loki/api/v1/query_range?"+q.Encode(), nil, "")
		if err != nil {
			return nil, err
		}
		var r struct {
			Data struct {
				Result []struct {
					Stream map[string]string `json:"stream"`
					Values [][2]string       `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		for _, s := range r.Data.Result {
			src := strings.Trim(s.Stream[be.field("namespace")]+"/"+s.Stream[be.field("pod")], "/")
			for _, v := range s.Values {
				ns, _ := strconv.ParseInt(v[0], 10, 64)
				lines = append(lines, LogLine{Time: time.Unix(0, ns), Source: src, Msg: v[1]})
			}
		}
	case "victorialogs":
		form := url.Values{"query": {query + fmt.Sprintf(" | sort by (_time desc) | limit %d", limit)}}
		data, err := l.request(ctx, be, http.MethodPost, base+"/select/logsql/query",
			strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 1<<20), 4<<20)
		for sc.Scan() {
			var rec map[string]string
			if json.Unmarshal(sc.Bytes(), &rec) != nil {
				continue
			}
			t, _ := time.Parse(time.RFC3339Nano, rec["_time"])
			src := strings.Trim(rec[be.field("namespace")]+"/"+rec[be.field("pod")], "/")
			lines = append(lines, LogLine{Time: t, Source: src, Msg: rec["_msg"]})
		}
	case "elasticsearch":
		ts := be.field("timestamp")
		body, _ := json.Marshal(map[string]any{
			"size": limit,
			"sort": []any{map[string]any{ts: map[string]any{"order": "desc"}}},
			"query": map[string]any{"bool": map[string]any{"filter": []any{
				map[string]any{"range": map[string]any{ts: map[string]any{"gte": fmt.Sprintf("now-%ds", int(since.Seconds()))}}},
				map[string]any{"query_string": map[string]any{"query": query, "analyze_wildcard": true}},
			}}},
		})
		index := be.Index
		if index == "" {
			index = "*"
		}
		data, err := l.request(ctx, be, http.MethodPost, base+"/"+url.PathEscape(index)+"/_search",
			bytes.NewReader(body), "application/json")
		if err != nil {
			return nil, err
		}
		var r struct {
			Hits struct {
				Hits []struct {
					Source map[string]any `json:"_source"`
				} `json:"hits"`
			} `json:"hits"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		for _, h := range r.Hits.Hits {
			t, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(dig(h.Source, ts)))
			src := strings.Trim(fmt.Sprint(dig(h.Source, be.field("namespace")))+"/"+fmt.Sprint(dig(h.Source, be.field("pod"))), "/")
			lines = append(lines, LogLine{Time: t, Source: strings.ReplaceAll(src, "<nil>", ""), Msg: fmt.Sprint(dig(h.Source, be.field("message")))})
		}
	default:
		return nil, fmt.Errorf("unsupported backend type %q", be.Type)
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Time.Before(lines[j].Time) })
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines, nil
}

// dig reads "a.b.c" from nested JSON objects, or a flat dotted key.
func dig(m map[string]any, path string) any {
	if v, ok := m[path]; ok {
		return v
	}
	var cur any = m
	for _, p := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[p]
	}
	return cur
}

func format(lines []LogLine, header string) string {
	if len(lines) == 0 {
		return header + "\nNo log lines."
	}
	var b strings.Builder
	b.WriteString(header + "\n")
	for _, l := range lines {
		fmt.Fprintf(&b, "%s [%s] %s\n", l.Time.UTC().Format(time.RFC3339), l.Source, strings.TrimRight(l.Msg, "\n"))
	}
	return b.String()
}

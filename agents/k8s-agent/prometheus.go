package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ventrolab/ai-sre/pkg/agentkit"
)

type PromConfig struct {
	URL             string `json:"url"`
	BearerTokenFile string `json:"bearerTokenFile"`
}

type Prometheus struct {
	cfg  PromConfig
	http *http.Client
}

type promResponse struct {
	Status    string `json:"status"`
	Error     string `json:"error"`
	ErrorType string `json:"errorType"`
	Data      struct {
		ResultType string            `json:"resultType"`
		Result     []json.RawMessage `json:"result"`
	} `json:"data"`
}

type promSeries struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
	Values [][]any           `json:"values"`
}

func (p *Prometheus) do(ctx context.Context, path string, q url.Values) (*promResponse, error) {
	if p.cfg.URL == "" {
		return nil, fmt.Errorf("no metrics backend is configured on this agent")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.cfg.URL, "/")+path,
		strings.NewReader(q.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if p.cfg.BearerTokenFile != "" {
		tok, err := os.ReadFile(p.cfg.BearerTokenFile)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("metrics backend returned %s", resp.Status)
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("%s: %s", pr.ErrorType, pr.Error)
	}
	return &pr, nil
}

func labels(m map[string]string) string {
	name := m["__name__"]
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "__name__" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, m[k])
	}
	return name + "{" + strings.Join(parts, ", ") + "}"
}

const maxSeries = 100

func (p *Prometheus) Query(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
		Time  string `json:"time"`
	}
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	q := url.Values{"query": {a.Query}}
	if a.Time != "" {
		q.Set("time", a.Time)
	}
	pr, err := p.do(ctx, "/api/v1/query", q)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	switch pr.Data.ResultType {
	case "scalar", "string":
		var v []any
		_ = json.Unmarshal(pr.Data.Result[0], &v)
		return fmt.Sprint(v...), nil
	}
	if len(pr.Data.Result) == 0 {
		return "Empty result: no series match.", nil
	}
	for i, r := range pr.Data.Result {
		if i == maxSeries {
			fmt.Fprintf(&b, "... %d more series; aggregate or filter the query\n", len(pr.Data.Result)-maxSeries)
			break
		}
		var s promSeries
		_ = json.Unmarshal(r, &s)
		if len(s.Value) == 2 {
			fmt.Fprintf(&b, "%s %v\n", labels(s.Metric), s.Value[1])
		}
	}
	return b.String(), nil
}

func (p *Prometheus) RangeQuery(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
		Since string `json:"since"`
		Step  string `json:"step"`
	}
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	window, err := agentkit.Since(a.Since, time.Hour, 7*24*time.Hour)
	if err != nil {
		return "", err
	}
	step := window / 60
	if a.Step != "" {
		if step, err = time.ParseDuration(a.Step); err != nil || step <= 0 {
			return "", fmt.Errorf("invalid step %q", a.Step)
		}
	}
	step = max(step, 15*time.Second)
	end := time.Now()
	q := url.Values{"query": {a.Query},
		"start": {strconv.FormatInt(end.Add(-window).Unix(), 10)}, "end": {strconv.FormatInt(end.Unix(), 10)},
		"step": {strconv.Itoa(int(step.Seconds()))}}
	pr, err := p.do(ctx, "/api/v1/query_range", q)
	if err != nil {
		return "", err
	}
	if len(pr.Data.Result) == 0 {
		return "Empty result: no series match.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "window %s, step %s; per series: first, min, max, last value and the time of the last point\n", window, step)
	for i, r := range pr.Data.Result {
		if i == maxSeries {
			fmt.Fprintf(&b, "... %d more series; aggregate or filter the query\n", len(pr.Data.Result)-maxSeries)
			break
		}
		var s promSeries
		_ = json.Unmarshal(r, &s)
		if len(s.Values) == 0 {
			continue
		}
		lo, hi := math.Inf(1), math.Inf(-1)
		val := func(p []any) float64 { f, _ := strconv.ParseFloat(fmt.Sprint(p[1]), 64); return f }
		for _, pt := range s.Values {
			v := val(pt)
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
		last := s.Values[len(s.Values)-1]
		ts, _ := last[0].(float64)
		fmt.Fprintf(&b, "%s first=%g min=%g max=%g last=%g at %s (%d points)\n", labels(s.Metric),
			val(s.Values[0]), lo, hi, val(last), time.Unix(int64(ts), 0).UTC().Format(time.RFC3339), len(s.Values))
	}
	return b.String(), nil
}

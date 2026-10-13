// k8s-agent: serves read-only Kubernetes, metrics and log tools of one cluster to the
// Tool Hub over an outbound WebSocket tunnel (agent token or mutual TLS).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	"github.com/ventrolab/ai-sre/pkg/agentkit"
)

var version = "dev"

type Config struct {
	agentkit.Common `json:",inline"`
	Prometheus      PromConfig   `json:"prometheus"`
	LogBackends     []LogBackend `json:"logBackends"`
	MaxLogWindow    string       `json:"maxLogWindow"`
	// Write primitives for the Action Guard; off unless enabled.
	Write struct {
		Enabled bool `json:"enabled"`
	} `json:"write"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "enroll-secret" {
		if err := enrollSecret(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "enroll-secret:", err)
			os.Exit(1)
		}
		return
	}
	path := flag.String("config", "/etc/ai-sre/agent.yaml", "config file")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	data, err := os.ReadFile(*path)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	maxWindow := 24 * time.Hour
	if cfg.MaxLogWindow != "" {
		if maxWindow, err = time.ParseDuration(cfg.MaxLogWindow); err != nil {
			log.Error("maxLogWindow", "err", err)
			os.Exit(1)
		}
	}
	rc, err := rest.InClusterConfig()
	if err != nil {
		rc, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	}
	if err != nil {
		log.Error("kubernetes config", "err", err)
		os.Exit(1)
	}
	rc.QPS, rc.Burst = 20, 40
	k8s, err := NewK8s(rc)
	if err != nil {
		log.Error("kubernetes client", "err", err)
		os.Exit(1)
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	prom := &Prometheus{cfg: cfg.Prometheus, http: hc}
	logs := &Logs{backends: cfg.LogBackends, http: hc, maxSince: maxWindow}

	toolset := map[string]agentkit.Tool{
		"k8s_list": k8s.List, "k8s_get": k8s.Get, "k8s_events": k8s.Events, "k8s_logs": k8s.Logs,
	}
	if cfg.Prometheus.URL != "" {
		toolset["prometheus_query"] = prom.Query
		toolset["prometheus_range_query"] = prom.RangeQuery
	}
	if len(cfg.LogBackends) > 0 {
		toolset["logs_backends"] = logs.Backends
		toolset["logs_query"] = logs.Query
		toolset["logs_native"] = logs.Native
	}
	if cfg.Write.Enabled {
		for name, fn := range (&Actions{cs: k8s.cs}).Toolset() {
			toolset[name] = fn
		}
	}
	if err := agentkit.Run("cluster", version, cfg.Common, toolset, log); err != nil {
		log.Error("agent", "err", err)
		os.Exit(1)
	}
}

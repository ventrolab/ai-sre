// host-agent: serves read-only diagnostics of a standalone host to the Tool Hub over an
// outbound WebSocket tunnel (agent token or mutual TLS). Runs as a systemd service.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/ventrolab/ai-sre/pkg/agentkit"
	"github.com/ventrolab/ai-sre/pkg/enroll"
)

var version = "dev"

type Config struct {
	agentkit.Common `json:",inline"`
	LogFiles        []string            `json:"logFiles"`
	Commands        map[string][]string `json:"commands"`
	MaxLogWindow    string              `json:"maxLogWindow"`
	// Write primitives for the Action Guard; off unless enabled.
	Write struct {
		Enabled    bool     `json:"enabled"`
		Systemctl  []string `json:"systemctl"`
		CleanPaths []string `json:"cleanPaths"`
	} `json:"write"`
}

func loadConfig(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	return cfg, yaml.UnmarshalStrict(data, &cfg)
}

// enrollCmd obtains the agent credential with a join token from the console:
//
//	host-agent enroll -config /etc/ai-sre/host-agent.yaml -token <join token> [-ca-sha256 <hex>]
//
// Token auth writes tokenFile (and tls.ca with a CA pin); mtls writes tls.ca, tls.cert and
// tls.key and needs the pin. Then start the service.
func enrollCmd(args []string) int {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	path := fs.String("config", "/etc/ai-sre/host-agent.yaml", "config file (hubURL, auth and credential paths)")
	token := fs.String("token", os.Getenv("AISRE_JOIN_TOKEN"), "one-time join token (or AISRE_JOIN_TOKEN)")
	caSHA := fs.String("ca-sha256", "", "SHA-256 of the hub CA certificate; needed for mtls and for a hub "+
		"endpoint without a publicly trusted certificate")
	_ = fs.Parse(args)
	cfg, err := loadConfig(*path)
	if err == nil && *token == "" {
		err = errors.New("-token is required")
	}
	if err == nil {
		cfg.Defaults()
		err = cfg.Validate()
	}
	if err == nil && *caSHA != "" && cfg.TLS.CA == "" {
		err = errors.New("-ca-sha256 needs tls.ca in the config (where the pinned CA is written)")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "enroll:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	name, err := enroll.Client(ctx, enroll.Options{HubURL: cfg.HubURL, JoinToken: *token, CASHA256: *caSHA,
		Auth: cfg.Auth, Files: enroll.Files{CA: cfg.TLS.CA, Cert: cfg.TLS.Cert, Key: cfg.TLS.Key,
			Token: cfg.TokenFile, ServerName: cfg.TLS.ServerName}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "enroll:", err)
		return 1
	}
	where := cfg.TokenFile
	if cfg.Auth == enroll.AuthMTLS {
		where = cfg.TLS.Cert
	}
	fmt.Printf("enrolled as %q (auth %s); credential written to %s\n", name, cfg.Auth, where)
	return 0
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		os.Exit(enrollCmd(os.Args[2:]))
	}
	path := flag.String("config", "/etc/ai-sre/host-agent.yaml", "config file")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := loadConfig(*path)
	if err != nil {
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
	h := &Host{LogFiles: cfg.LogFiles, Commands: cfg.Commands, MaxSince: maxWindow}
	toolset := map[string]agentkit.Tool{
		"host_info": h.Info, "host_processes": h.Processes, "host_service_status": h.ServiceStatus,
		"host_journal": h.Journal, "host_log_file": h.LogFile, "host_command": h.Command,
	}
	if cfg.Write.Enabled {
		for name, fn := range (&HostActions{Systemctl: cfg.Write.Systemctl, CleanPaths: cfg.Write.CleanPaths}).Toolset() {
			toolset[name] = fn
		}
	}
	if err := agentkit.Run("host", version, cfg.Common, toolset, log); err != nil {
		log.Error("agent", "err", err)
		os.Exit(1)
	}
}

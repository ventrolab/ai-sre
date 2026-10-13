// aisre: terminal client of the AI SRE web console API (/api/v1). Shows the system status,
// incidents with their RCA, approvals and agents. Authenticates with an API token
// (console: Administration -> API tokens, scope status:read; agents:read for "agents").
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"
	"sigs.k8s.io/yaml"
)

var version = "dev"

const usage = `aisre - AI SRE from the terminal

Usage:
  aisre login [--url URL]          save the console URL and an API token (asked for)
  aisre status                     remediation mode, LLM, incidents, approvals, components
  aisre incidents [-a] [-s STATUS] [-q TEXT] [-n N]
                                   open incidents (-a: all, -s: one status, -q: search)
  aisre incident ID                one incident: alerts, root cause, analysis, timeline
  aisre approvals                  pending approvals and recent decisions
  aisre agents                     agents connected to the Tool Hub and their state
  aisre version

Global flags (any command):
  --url URL        console URL (AISRE_URL, config file)
  --token TOKEN    API token (AISRE_TOKEN, config file)
  -o table|json    output format (default table)

Config file: $XDG_CONFIG_HOME/aisre/config.yaml or ~/.config/aisre/config.yaml.
"status" exits with code 2 when a component is unhealthy.
`

type Config struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "aisre", "config.yaml")
}

func loadConfig() Config {
	var c Config
	if data, err := os.ReadFile(configPath()); err == nil {
		_ = yaml.Unmarshal(data, &c)
	}
	return c
}

func saveConfig(c Config) error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// globals are accepted by every command.
type globals struct {
	url, token, output string
}

func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.url, "url", "", "console URL")
	fs.StringVar(&g.token, "token", "", "API token")
	fs.StringVar(&g.output, "o", "table", "output format: table or json")
}

// resolve fills URL and token from flags, the environment and the config file.
func (g *globals) resolve() error {
	cfg := loadConfig()
	if g.url == "" {
		g.url = firstNonEmpty(os.Getenv("AISRE_URL"), cfg.URL)
	}
	if g.token == "" {
		g.token = firstNonEmpty(os.Getenv("AISRE_TOKEN"), cfg.Token)
	}
	if g.url == "" || g.token == "" {
		return errors.New("console URL and API token are needed: run 'aisre login', or set AISRE_URL and AISRE_TOKEN")
	}
	if g.output != "table" && g.output != "json" {
		return fmt.Errorf("-o must be table or json, got %q", g.output)
	}
	g.url = strings.TrimRight(g.url, "/")
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Client calls the console API.
type Client struct {
	base, token string
	http        *http.Client
}

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg) }

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.base + "/api/v1" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "aisre/"+version)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
			if len(e.Error) > 200 || strings.HasPrefix(e.Error, "<") {
				e.Error = http.StatusText(resp.StatusCode) + " (is the URL the AI SRE console?)"
			}
		}
		return &apiError{resp.StatusCode, e.Error}
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = data
		return nil
	}
	return json.Unmarshal(data, out)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	if cmd == "version" || cmd == "--version" {
		fmt.Fprintln(stdout, "aisre", version)
		return 0
	}
	if cmd == "login" {
		return login(rest, stdout, stderr)
	}
	handlers := map[string]func(*command) error{
		"status": cmdStatus, "incidents": cmdIncidents, "incident": cmdIncident,
		"approvals": cmdApprovals, "agents": cmdAgents,
	}
	h, ok := handlers[cmd]
	if !ok {
		fmt.Fprintf(stderr, "aisre: unknown command %q\n\n%s", cmd, usage)
		return 1
	}
	c := &command{name: cmd, fs: flag.NewFlagSet(cmd, flag.ContinueOnError), out: stdout, raw: rest}
	c.fs.SetOutput(stderr)
	c.g.register(c.fs)
	if err := h(c); err != nil {
		var exit exitCode
		if errors.As(err, &exit) {
			return int(exit)
		}
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "aisre:", err)
		var ae *apiError
		if errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403) {
			fmt.Fprintln(stderr, "aisre: the API token needs the scope status:read (agents: agents:read)")
		}
		return 1
	}
	return 0
}

type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// command is one invocation: its flags, the resolved globals and the client.
type command struct {
	name string
	fs   *flag.FlagSet
	g    globals
	out  io.Writer
	cl   *Client
	raw  []string // arguments after the command name
	args []string // positional arguments
}

// parse parses flags (globals may follow the command) and builds the client.
func (c *command) parse(args []string) error {
	// Allow flags after positional arguments: "aisre incident ID -o json".
	for {
		if err := c.fs.Parse(args); err != nil {
			return err
		}
		if c.fs.NArg() == 0 {
			break
		}
		c.args = append(c.args, c.fs.Arg(0))
		args = c.fs.Args()[1:]
	}
	if err := c.g.resolve(); err != nil {
		return err
	}
	c.cl = &Client{base: c.g.url, token: c.g.token, http: &http.Client{Timeout: 30 * time.Second}}
	return nil
}

func (c *command) ctx() context.Context { return context.Background() }

func (c *command) json(raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	enc := json.NewEncoder(c.out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func login(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	u := fs.String("url", "", "console URL, e.g. https://ai-sre.example.com")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	cfg := loadConfig()
	in := bufio.NewReader(os.Stdin)
	if *u == "" {
		*u = os.Getenv("AISRE_URL")
	}
	if *u == "" {
		fmt.Fprint(stderr, "Console URL")
		if cfg.URL != "" {
			fmt.Fprintf(stderr, " [%s]", cfg.URL)
		}
		fmt.Fprint(stderr, ": ")
		line, _ := in.ReadString('\n')
		*u = firstNonEmpty(strings.TrimSpace(line), cfg.URL)
	}
	token := os.Getenv("AISRE_TOKEN")
	if token == "" {
		fmt.Fprint(stderr, "API token (scope status:read): ")
		if term.IsTerminal(int(os.Stdin.Fd())) {
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(stderr)
			if err != nil {
				fmt.Fprintln(stderr, "aisre:", err)
				return 1
			}
			token = string(b)
		} else {
			line, _ := in.ReadString('\n')
			token = line
		}
		token = strings.TrimSpace(token)
	}
	if *u == "" || token == "" {
		fmt.Fprintln(stderr, "aisre: URL and token are required")
		return 1
	}
	cl := &Client{base: strings.TrimRight(*u, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
	var st statusResp
	if err := cl.get(context.Background(), "/status", nil, &st); err != nil {
		fmt.Fprintln(stderr, "aisre: check failed:", err)
		return 1
	}
	if err := saveConfig(Config{URL: cl.base, Token: token}); err != nil {
		fmt.Fprintln(stderr, "aisre:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Logged in to %s (AI SRE %s, cluster %s); saved to %s\n", cl.base, st.Version, st.Cluster, configPath())
	return 0
}

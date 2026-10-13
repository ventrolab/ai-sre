package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

type component struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type statusResp struct {
	Version     string `json:"version"`
	Cluster     string `json:"cluster"`
	Remediation struct {
		Mode              string `json:"mode"`
		MaxRisk           string `json:"max_risk"`
		MaxActionsPerHour *int   `json:"max_actions_per_hour"`
		Playbooks         *int   `json:"playbooks"`
		Available         bool   `json:"available"`
	} `json:"remediation"`
	LLM struct {
		Provider  string   `json:"provider"`
		Fallbacks []string `json:"fallbacks"`
	} `json:"llm"`
	Incidents struct {
		Open        int            `json:"open"`
		ByStatus    map[string]int `json:"by_status"`
		Opened24h   int            `json:"opened_24h"`
		Opened7d    int            `json:"opened_7d"`
		MTTDSeconds *float64       `json:"mttd_seconds"`
		MTTRSeconds *float64       `json:"mttr_seconds"`
		TopAlerts   []struct {
			Alertname string `json:"alertname"`
			N         int    `json:"n"`
		} `json:"top_alerts_7d"`
	} `json:"incidents"`
	PendingApprovals int         `json:"pending_approvals"`
	Components       []component `json:"components"`
}

type target map[string]any

// describe renders a target like the console does: the most specific object first.
func (t target) describe() string {
	get := func(k string) string {
		if v, ok := t[k].(string); ok {
			return v
		}
		return ""
	}
	var parts []string
	if h := get("host"); h != "" {
		parts = append(parts, "host/"+h)
	}
	if ns := get("namespace"); ns != "" {
		obj := firstNonEmpty(get("workload"), get("pod"))
		if obj != "" {
			parts = append(parts, ns+"/"+obj)
		} else {
			parts = append(parts, "ns/"+ns)
		}
	} else if n := get("node"); n != "" {
		parts = append(parts, "node/"+n)
	}
	if cl := get("cluster"); cl != "" && get("host") == "" {
		parts = append([]string{cl}, parts...)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

type incident struct {
	ID         string     `json:"id"`
	Key        string     `json:"key"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	Title      string     `json:"title"`
	Confidence *float64   `json:"confidence"`
	Playbook   *string    `json:"playbook"`
	Target     target     `json:"target"`
	AlertCount int        `json:"alert_count"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ResolvedAt *time.Time `json:"resolved_at"`
	RootCause  *string    `json:"root_cause"`
	Analysis   *string    `json:"analysis"`
	Alerts     []struct {
		Alertname   string            `json:"alertname"`
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    string            `json:"starts_at"`
	} `json:"alerts"`
	Events []struct {
		TS   time.Time       `json:"ts"`
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	} `json:"events"`
	Approvals []approval `json:"approvals"`
}

type approval struct {
	ID          string     `json:"id"`
	IncidentID  string     `json:"incident_id"`
	Title       string     `json:"title"`
	Playbook    string     `json:"playbook"`
	Status      string     `json:"status"`
	RequestedAt time.Time  `json:"requested_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	DecidedAt   *time.Time `json:"decided_at"`
	DecidedBy   *string    `json:"decided_by"`
}

func table(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ago is a compact age: 45s, 12m, 3h, 5d.
func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return compact(time.Since(t)) + " ago"
}

func compact(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < 10*time.Minute && int(d.Seconds())%60 != 0:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func seconds(v *float64) string {
	if v == nil {
		return "-"
	}
	return compact(time.Duration(*v * float64(time.Second)))
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func cmdStatus(c *command) error {
	if err := c.parse(c.raw); err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.cl.get(c.ctx(), "/status", nil, &raw); err != nil {
		return err
	}
	var st statusResp
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	unhealthy := 0
	for _, comp := range st.Components {
		if !comp.OK {
			unhealthy++
		}
	}
	if c.g.output == "json" {
		if err := c.json(raw); err != nil {
			return err
		}
	} else {
		printStatus(c.out, c.g.url, st)
	}
	if unhealthy > 0 {
		return exitCode(2)
	}
	return nil
}

func printStatus(w io.Writer, base string, st statusResp) {
	tw := table(w)
	fmt.Fprintf(tw, "AI SRE\t%s, cluster %s\t%s\n", st.Version, st.Cluster, base)
	r := st.Remediation
	if r.Available {
		var extra []string
		if r.MaxRisk != "" {
			extra = append(extra, "max risk "+r.MaxRisk)
		}
		if r.MaxActionsPerHour != nil {
			extra = append(extra, fmt.Sprintf("%d actions/h per target", *r.MaxActionsPerHour))
		}
		if r.Playbooks != nil {
			extra = append(extra, fmt.Sprintf("%d playbooks", *r.Playbooks))
		}
		fmt.Fprintf(tw, "Remediation\t%s\t%s\n", strings.ToUpper(r.Mode), strings.Join(extra, ", "))
	} else {
		fmt.Fprintf(tw, "Remediation\tunknown\tAction Guard not reachable\n")
	}
	llm := st.LLM.Provider
	if len(st.LLM.Fallbacks) > 0 {
		llm += ", fallbacks " + strings.Join(st.LLM.Fallbacks, ", ")
	}
	fmt.Fprintf(tw, "LLM\t%s\t\n", llm)
	var states []string
	for s, n := range st.Incidents.ByStatus {
		if s != "resolved" && s != "closed" && n > 0 {
			states = append(states, fmt.Sprintf("%s %d", s, n))
		}
	}
	sort.Strings(states)
	fmt.Fprintf(tw, "Incidents\t%d open\t%s\n", st.Incidents.Open, strings.Join(states, ", "))
	fmt.Fprintf(tw, "Opened\t%d in 24h, %d in 7d\t\n", st.Incidents.Opened24h, st.Incidents.Opened7d)
	fmt.Fprintf(tw, "MTTD / MTTR\t%s / %s\t7 days\n", seconds(st.Incidents.MTTDSeconds), seconds(st.Incidents.MTTRSeconds))
	fmt.Fprintf(tw, "Approvals\t%d pending\t\n", st.PendingApprovals)
	tw.Flush()

	fmt.Fprintln(w, "\nComponents")
	tw = table(w)
	for _, comp := range st.Components {
		state, detail := "ok", ""
		if !comp.OK {
			state, detail = "FAIL", comp.Detail
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", state, comp.Name, detail)
	}
	tw.Flush()
	if len(st.Incidents.TopAlerts) > 0 {
		fmt.Fprintln(w, "\nTop alerts, 7 days")
		tw = table(w)
		for _, a := range st.Incidents.TopAlerts {
			fmt.Fprintf(tw, "  %d\t%s\n", a.N, a.Alertname)
		}
		tw.Flush()
	}
}

func cmdIncidents(c *command) error {
	all := c.fs.Bool("a", false, "all incidents, not only open ones")
	status := c.fs.String("s", "", "only this status (diagnosed, resolved, ...)")
	q := c.fs.String("q", "", "search in title, key and id")
	n := c.fs.Int("n", 50, "at most N incidents")
	if err := c.parse(c.raw); err != nil {
		return err
	}
	query := url.Values{"status": {"open"}, "limit": {fmt.Sprint(*n)}}
	if *all {
		query.Set("status", "all")
	}
	if *status != "" {
		query.Set("status", *status)
	}
	if *q != "" {
		query.Set("q", *q)
	}
	var raw json.RawMessage
	if err := c.cl.get(c.ctx(), "/incidents", query, &raw); err != nil {
		return err
	}
	if c.g.output == "json" {
		return c.json(raw)
	}
	var resp struct {
		Incidents []incident `json:"incidents"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if len(resp.Incidents) == 0 {
		fmt.Fprintln(c.out, "No incidents.")
		return nil
	}
	tw := table(c.out)
	fmt.Fprintln(tw, "ID\tSTATUS\tUPDATED\tCONF\tTARGET\tTITLE")
	for _, i := range resp.Incidents {
		conf := "-"
		if i.Confidence != nil {
			conf = fmt.Sprintf("%.0f%%", *i.Confidence*100)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", short(i.ID), i.Status, ago(i.UpdatedAt), conf,
			clip(i.Target.describe(), 40), clip(i.Title, 60))
	}
	return tw.Flush()
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// resolveID turns an id prefix (as "aisre incidents" prints it) into the full id.
func (c *command) resolveID(prefix string) (string, error) {
	if uuidRe.MatchString(prefix) {
		return prefix, nil
	}
	if len(prefix) < 4 {
		return "", errors.New("give at least 4 characters of the incident id")
	}
	var resp struct {
		Incidents []incident `json:"incidents"`
	}
	if err := c.cl.get(c.ctx(), "/incidents", url.Values{"status": {"all"}, "q": {prefix}, "limit": {"500"}}, &resp); err != nil {
		return "", err
	}
	var found []string
	for _, i := range resp.Incidents {
		if strings.HasPrefix(i.ID, strings.ToLower(prefix)) {
			found = append(found, i.ID)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no incident with id %s", prefix)
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("id %s is ambiguous (%d incidents), give more characters", prefix, len(found))
	}
}

func cmdIncident(c *command) error {
	if err := c.parse(c.raw); err != nil {
		return err
	}
	if len(c.args) != 1 {
		return errors.New("usage: aisre incident ID")
	}
	id, err := c.resolveID(c.args[0])
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.cl.get(c.ctx(), "/incidents/"+url.PathEscape(id), nil, &raw); err != nil {
		return err
	}
	if c.g.output == "json" {
		return c.json(raw)
	}
	var i incident
	if err := json.Unmarshal(raw, &i); err != nil {
		return err
	}
	printIncident(c.out, c.g.url, i)
	return nil
}

func printIncident(w io.Writer, base string, i incident) {
	tw := table(w)
	fmt.Fprintf(tw, "Incident\t%s\n", i.Title)
	fmt.Fprintf(tw, "ID\t%s\n", i.ID)
	fmt.Fprintf(tw, "Status\t%s\n", i.Status)
	fmt.Fprintf(tw, "Target\t%s\n", i.Target.describe())
	opened := i.CreatedAt.Local().Format("2006-01-02 15:04")
	if i.ResolvedAt != nil {
		opened += fmt.Sprintf(", resolved after %s", compact(i.ResolvedAt.Sub(i.CreatedAt)))
	} else {
		opened += fmt.Sprintf(" (%s)", ago(i.CreatedAt))
	}
	fmt.Fprintf(tw, "Opened\t%s\n", opened)
	if i.Confidence != nil {
		fmt.Fprintf(tw, "Confidence\t%.0f%%\n", *i.Confidence*100)
	}
	if i.Playbook != nil && *i.Playbook != "" {
		fmt.Fprintf(tw, "Playbook\t%s\n", *i.Playbook)
	}
	fmt.Fprintf(tw, "Console\t%s/incidents/%s\n", base, i.ID)
	tw.Flush()

	if len(i.Alerts) > 0 {
		fmt.Fprintln(w, "\nAlerts")
		tw = table(w)
		for _, a := range i.Alerts {
			summary := firstNonEmpty(a.Annotations["summary"], a.Annotations["description"])
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", a.Status, a.Alertname, clip(summary, 90))
		}
		tw.Flush()
	}
	if i.RootCause != nil && *i.RootCause != "" {
		fmt.Fprintf(w, "\nRoot cause\n  %s\n", strings.ReplaceAll(strings.TrimSpace(*i.RootCause), "\n", "\n  "))
	}
	if i.Analysis != nil && *i.Analysis != "" {
		fmt.Fprintf(w, "\nAnalysis\n%s\n", strings.TrimSpace(*i.Analysis))
	}
	if len(i.Approvals) > 0 {
		fmt.Fprintln(w, "\nApprovals")
		tw = table(w)
		for _, a := range i.Approvals {
			by := ""
			if a.DecidedBy != nil {
				by = "by " + *a.DecidedBy
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", short(a.ID), a.Playbook, a.Status, by)
		}
		tw.Flush()
	}
	if len(i.Events) > 0 {
		fmt.Fprintln(w, "\nTimeline")
		tw = table(w)
		for _, e := range i.Events {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", e.TS.Local().Format("01-02 15:04:05"), e.Type, clip(eventSummary(e.Data), 90))
		}
		tw.Flush()
	}
}

// eventSummary shows the short fields of an event's data.
func eventSummary(data json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(data, &m) != nil || len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if len(v) <= 80 {
				parts = append(parts, k+"="+v)
			}
		case float64, bool:
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return strings.Join(parts, " ")
}

func cmdApprovals(c *command) error {
	if err := c.parse(c.raw); err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.cl.get(c.ctx(), "/approvals", nil, &raw); err != nil {
		return err
	}
	if c.g.output == "json" {
		return c.json(raw)
	}
	var resp struct {
		Pending []approval `json:"pending"`
		History []approval `json:"history"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if len(resp.Pending) == 0 {
		fmt.Fprintln(c.out, "No pending approvals.")
	} else {
		fmt.Fprintln(c.out, "Pending (approve in chat or the console)")
		tw := table(c.out)
		fmt.Fprintln(tw, "ID\tINCIDENT\tPLAYBOOK\tREQUESTED\tEXPIRES IN\tTITLE")
		for _, a := range resp.Pending {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", short(a.ID), short(a.IncidentID), a.Playbook, ago(a.RequestedAt),
				compact(time.Until(a.ExpiresAt)), clip(a.Title, 50))
		}
		tw.Flush()
	}
	if len(resp.History) > 0 {
		fmt.Fprintln(c.out, "\nRecent decisions")
		tw := table(c.out)
		fmt.Fprintln(tw, "ID\tINCIDENT\tPLAYBOOK\tSTATUS\tBY\tWHEN\tTITLE")
		for _, a := range resp.History {
			by, when := "-", ago(a.RequestedAt)
			if a.DecidedBy != nil {
				by = *a.DecidedBy
			}
			if a.DecidedAt != nil {
				when = ago(*a.DecidedAt)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", short(a.ID), short(a.IncidentID), a.Playbook, a.Status, by,
				when, clip(a.Title, 50))
		}
		tw.Flush()
	}
	return nil
}

func cmdAgents(c *command) error {
	if err := c.parse(c.raw); err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.cl.get(c.ctx(), "/agents", nil, &raw); err != nil {
		return err
	}
	if c.g.output == "json" {
		return c.json(raw)
	}
	var resp struct {
		Agents []struct {
			Name       string            `json:"name"`
			Kind       string            `json:"kind"`
			State      string            `json:"state"`
			Auth       *string           `json:"auth"`
			Version    *string           `json:"version"`
			Labels     map[string]string `json:"labels"`
			Tools      []string          `json:"tools"`
			LastSeenAt *time.Time        `json:"last_seen_at"`
			RevokedAt  *time.Time        `json:"revoked_at"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if len(resp.Agents) == 0 {
		fmt.Fprintln(c.out, "No agents.")
		return nil
	}
	tw := table(c.out)
	fmt.Fprintln(tw, "NAME\tKIND\tSTATE\tAUTH\tVERSION\tTOOLS\tLAST SEEN\tLABELS")
	for _, a := range resp.Agents {
		state := a.State
		if a.RevokedAt != nil {
			state += ",revoked"
		}
		auth, ver, seen := "-", "-", "-"
		if a.Auth != nil && *a.Auth != "" {
			auth = *a.Auth
		}
		if a.Version != nil && *a.Version != "" {
			ver = *a.Version
		}
		if a.LastSeenAt != nil {
			seen = ago(*a.LastSeenAt)
		}
		keys := make([]string, 0, len(a.Labels))
		for k := range a.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var labels []string
		for _, k := range keys {
			labels = append(labels, k+"="+a.Labels[k])
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", a.Name, a.Kind, state, auth, ver, len(a.Tools), seen,
			strings.Join(labels, ","))
	}
	return tw.Flush()
}

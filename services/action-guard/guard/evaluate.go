package guard

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Target struct {
	Kind      string            `json:"kind"`
	Cluster   string            `json:"cluster,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	Workload  string            `json:"workload,omitempty"`
	Pod       string            `json:"pod,omitempty"`
	Node      string            `json:"node,omitempty"`
	Host      string            `json:"host,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// Key identifies the object for rate limiting.
func (t Target) Key() string {
	return strings.Join([]string{t.Cluster, t.Namespace, t.Workload, t.Pod, t.Node, t.Host}, "/")
}

type AlertContext struct {
	Name   string            `json:"alertname,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

type EvalRequest struct {
	IncidentID string       `json:"incident_id"`
	Playbook   string       `json:"playbook"`
	Target     Target       `json:"target"`
	Alert      AlertContext `json:"alert"`
	ApprovedBy string       `json:"approved_by,omitempty"`
}

type RenderedStep struct {
	Primitive string         `json:"primitive"`
	Params    map[string]any `json:"params,omitempty"`
}

type Decision string

const (
	DecisionPropose  Decision = "propose"
	DecisionApproval Decision = "approval_required"
	DecisionExecute  Decision = "execute"
	DecisionDeny     Decision = "deny"
)

type EvalResponse struct {
	Decision       Decision       `json:"decision"`
	Reason         string         `json:"reason"`
	Mode           Mode           `json:"mode"`
	Playbook       string         `json:"playbook"`
	Description    string         `json:"description,omitempty"`
	Risk           string         `json:"risk,omitempty"`
	CatalogVersion string         `json:"catalog_version"`
	Action         *RenderedStep  `json:"action,omitempty"`
	Precheck       []RenderedStep `json:"precheck,omitempty"`
	Rollback       *RenderedStep  `json:"rollback,omitempty"`
	Verify         *Verify        `json:"verify,omitempty"`
}

type Mode string

const (
	ModeReadonly Mode = "readonly"
	ModeAutofix  Mode = "autofix"
)

type Policy struct {
	Mode                    Mode
	MaxRisk                 Risk
	MaxActionsPerTargetHour int
	// Decider makes the final decision; nil means CommunityDecider.
	Decider Decider
}

func (p Policy) decider() Decider {
	if p.Decider == nil {
		return CommunityDecider{}
	}
	return p.Decider
}

// Decider makes the final decision for a playbook that exists, applies to the target and
// rendered without errors. Editions differ only here.
type Decider interface {
	Decide(pb *Playbook, pol Policy, rl Limiter, req EvalRequest) (Decision, string)
	// MaxAutoRisk is the highest risk that may run without approval under pol.
	MaxAutoRisk(pol Policy) Risk
}

// CommunityDecider is the open-source policy: readonly proposes, autofix runs playbooks up
// to risk low within the rate limits; anything riskier waits for approval.
type CommunityDecider struct{}

func (CommunityDecider) MaxAutoRisk(pol Policy) Risk {
	return min(pol.MaxRisk, RiskLow)
}

func (d CommunityDecider) Decide(pb *Playbook, pol Policy, rl Limiter, req EvalRequest) (Decision, string) {
	switch auto := d.MaxAutoRisk(pol); {
	case pol.Mode == ModeReadonly:
		return DecisionPropose, "SRE_MODE=readonly: actions are proposed, never executed"
	case pb.Risk() == RiskHigh:
		return DecisionApproval, "high-risk playbooks always need human approval"
	case pb.Risk() > auto && pb.Risk() <= pol.MaxRisk:
		return DecisionApproval, fmt.Sprintf("risk %s needs approval: autofix above risk low is part of the enterprise edition", pb.Risk())
	case pb.Risk() > auto:
		return DecisionApproval, fmt.Sprintf("risk %s is above AUTOFIX_MAX_RISK=%s", pb.Risk(), pol.MaxRisk)
	default:
		return RateLimited(pb, pol, rl, req, auto)
	}
}

// RateLimited executes unless the target had too many actions in the last hour.
func RateLimited(pb *Playbook, pol Policy, rl Limiter, req EvalRequest, auto Risk) (Decision, string) {
	limit := pol.MaxActionsPerTargetHour
	if l := pb.Spec.Limits.MaxPerTargetPerHour; l > 0 && l < limit {
		limit = l
	}
	if n := rl.Count(req.Target.Key()); n >= limit {
		return DecisionApproval, fmt.Sprintf("rate limit: %d actions on this target in the last hour (limit %d)", n, limit)
	}
	return DecisionExecute, fmt.Sprintf("risk %s within the autofix limit %s", pb.Risk(), auto)
}

// Limiter counts executed actions per target in a sliding hour.
type Limiter interface {
	Count(key string) int
	Record(key string)
}

// RateLimiter is the in-memory Limiter: lost on restart and not shared between replicas.
type RateLimiter struct {
	mu     sync.Mutex
	events map[string][]time.Time
	now    func() time.Time
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{events: map[string][]time.Time{}, now: time.Now}
}

func (r *RateLimiter) Count(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := r.now().Add(-time.Hour)
	kept := r.events[key][:0]
	for _, t := range r.events[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	r.events[key] = kept
	return len(kept)
}

func (r *RateLimiter) Record(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events[key] = append(r.events[key], r.now())
}

// Evaluate decides what may happen with a playbook for a target.
// It never executes anything.
func Evaluate(cat *Catalog, pol Policy, rl Limiter, req EvalRequest) EvalResponse {
	resp := EvalResponse{Mode: pol.Mode, Playbook: req.Playbook, CatalogVersion: cat.Version}
	deny := func(format string, args ...any) EvalResponse {
		resp.Decision = DecisionDeny
		resp.Reason = fmt.Sprintf(format, args...)
		return resp
	}

	pb, ok := cat.Playbooks[req.Playbook]
	if !ok {
		return deny("unknown playbook %q", req.Playbook)
	}
	resp.Description = pb.Spec.Description
	resp.Risk = pb.Risk().String()
	if !pb.Enabled() {
		return deny("playbook %s is disabled", pb.Name())
	}
	if err := matches(pb, req); err != nil {
		return deny("playbook %s does not apply: %v", pb.Name(), err)
	}

	ctx := TemplateContext{Target: req.Target, Alert: req.Alert}
	action, err := renderStep(ctx, pb.Spec.Action)
	if err != nil {
		return deny("action: %v", err)
	}
	resp.Action = action
	for i, s := range pb.Spec.Precheck {
		r, err := renderStep(ctx, s)
		if err != nil {
			return deny("precheck[%d]: %v", i, err)
		}
		// A check without params is about the action's object: inherit the
		// action params the check primitive declares.
		if len(r.Params) == 0 {
			for name := range primitives[r.Primitive].Params {
				if v, ok := action.Params[name]; ok {
					r.Params[name] = v
				}
			}
		}
		resp.Precheck = append(resp.Precheck, *r)
	}
	if pb.Spec.Rollback.Step != nil {
		r, err := renderStep(ctx, *pb.Spec.Rollback.Step)
		if err != nil {
			return deny("rollback: %v", err)
		}
		resp.Rollback = r
	}
	if v := pb.Spec.Verify; v != nil {
		q, err := ctx.Render(v.PromQL)
		if err != nil {
			return deny("verify: %v", err)
		}
		resp.Verify = &Verify{PromQL: strings.TrimSpace(q), Timeout: v.Timeout}
	}

	resp.Decision, resp.Reason = pol.decider().Decide(pb, pol, rl, req)
	return resp
}

func matches(pb *Playbook, req EvalRequest) error {
	m := pb.Spec.Match
	if len(m.Alertnames) > 0 && !contains(m.Alertnames, req.Alert.Name) {
		return fmt.Errorf("alert %q is not in %v", req.Alert.Name, m.Alertnames)
	}
	if m.TargetKind != "" && m.TargetKind != req.Target.Kind {
		return fmt.Errorf("target kind %q, want %q", req.Target.Kind, m.TargetKind)
	}
	for label, allowed := range m.Labels {
		if !contains(allowed, req.Target.Labels[label]) {
			return fmt.Errorf("target label %s=%q is out of scope %v", label, req.Target.Labels[label], allowed)
		}
	}
	return nil
}

func renderStep(ctx TemplateContext, s Step) (*RenderedStep, error) {
	p := primitives[s.Primitive]
	out := &RenderedStep{Primitive: s.Primitive, Params: map[string]any{}}
	for name, value := range s.Params {
		spec := p.Params[name]
		switch v := value.(type) {
		case string:
			r, err := ctx.Render(v)
			if err != nil {
				return nil, fmt.Errorf("param %s: %w", name, err)
			}
			if spec.Type == ParamInt {
				n, err := strconv.Atoi(r)
				if err != nil {
					return nil, fmt.Errorf("param %s: %q is not an integer", name, r)
				}
				out.Params[name] = n
			} else {
				out.Params[name] = r
			}
		default:
			out.Params[name] = v
		}
	}
	return out, nil
}

// Matching lists enabled playbooks applicable to an alert and target kind.
func Matching(cat *Catalog, alertname, targetKind string) []*Playbook {
	var out []*Playbook
	for _, pb := range cat.Sorted() {
		if !pb.Enabled() {
			continue
		}
		m := pb.Spec.Match
		if alertname != "" && len(m.Alertnames) > 0 && !contains(m.Alertnames, alertname) {
			continue
		}
		if targetKind != "" && m.TargetKind != "" && m.TargetKind != targetKind {
			continue
		}
		out = append(out, pb)
	}
	return out
}

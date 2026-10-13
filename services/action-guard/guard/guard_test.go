package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCatalog(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const podRestart = `
apiVersion: ai-sre.io/v1
kind: Playbook
metadata:
  name: restart-crashlooping-pod
spec:
  description: Delete a crash-looping pod
  risk: low
  match:
    alertnames: [KubePodCrashLooping]
    targetKind: k8s.pod
  action:
    primitive: k8s.pod.delete
    params:
      namespace: "{{ target.namespace }}"
      pod: "{{ target.pod }}"
  precheck:
    - primitive: k8s.pod.ownerExists
  verify:
    promql: 'up{namespace="{{ target.namespace }}"} == 1'
    timeout: 10m
  rollback: none
  limits:
    maxPerTargetPerHour: 2
`

const scaleUp = `
apiVersion: ai-sre.io/v1
kind: Playbook
metadata:
  name: scale-up
spec:
  risk: medium
  match:
    targetKind: k8s.deployment
  action:
    primitive: k8s.deployment.scale
    params:
      namespace: "{{ target.namespace }}"
      deployment: "{{ target.workload }}"
      delta: 1
  rollback:
    primitive: k8s.deployment.scale
    params:
      namespace: "{{ target.namespace }}"
      deployment: "{{ target.workload }}"
      delta: -1
`

func TestRepositoryDefaultCatalogIsValid(t *testing.T) {
	cat, err := LoadCatalog("../../../playbooks/default")
	if err != nil {
		t.Fatalf("default catalog: %v", err)
	}
	if len(cat.Playbooks) == 0 {
		t.Fatal("default catalog is empty")
	}
}

func TestCatalogRejectsInvalidPlaybooks(t *testing.T) {
	cases := map[string]string{
		"unknown primitive": strings.Replace(podRestart, "k8s.pod.delete", "k8s.pod.explode", 1),
		"risk below minimum": strings.Replace(
			strings.Replace(scaleUp, "risk: medium", "risk: low", 1), "", "", 1),
		"unknown variable": strings.Replace(podRestart, "target.pod }}", "target.secret }}", 1),
		"expression":       strings.Replace(podRestart, "{{ target.pod }}", "{{ target.pod | upper }}", 1),
		"missing rollback": strings.Replace(podRestart, "rollback: none\n", "", 1),
		"unknown field":    strings.Replace(podRestart, "risk: low", "risk: low\n  shell: rm -rf /", 1),
		"check as action":  strings.Replace(podRestart, "primitive: k8s.pod.delete", "primitive: k8s.pod.ownerExists", 1),
		"bad verify":       strings.Replace(podRestart, "timeout: 10m", "timeout: soon", 1),
		"missing param":    strings.Replace(podRestart, "      pod: \"{{ target.pod }}\"\n", "", 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadCatalog(writeCatalog(t, map[string]string{"p.yaml": body})); err == nil {
				t.Fatal("expected the catalog to be rejected")
			}
		})
	}
}

func TestCatalogRejectsDuplicates(t *testing.T) {
	dir := writeCatalog(t, map[string]string{"a.yaml": podRestart, "b.yaml": podRestart})
	if _, err := LoadCatalog(dir); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestCatalogVersionChangesOnEdit(t *testing.T) {
	dir := writeCatalog(t, map[string]string{"p.yaml": podRestart})
	v1, _ := CatalogVersion(dir)
	_ = os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(podRestart+"\n"), 0o644)
	v2, _ := CatalogVersion(dir)
	if v1 == v2 {
		t.Fatal("version did not change")
	}
}

func testCatalog(t *testing.T) *Catalog {
	cat, err := LoadCatalog(writeCatalog(t, map[string]string{"a.yaml": podRestart, "b.yaml": scaleUp}))
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

var podReq = EvalRequest{
	IncidentID: "i1",
	Playbook:   "restart-crashlooping-pod",
	Target:     Target{Kind: "k8s.pod", Cluster: "c", Namespace: "demo", Workload: "crashy", Pod: "crashy-abc"},
	Alert:      AlertContext{Name: "KubePodCrashLooping"},
}

func TestReadonlyOnlyProposes(t *testing.T) {
	resp := Evaluate(testCatalog(t), Policy{Mode: ModeReadonly, MaxRisk: RiskMedium, MaxActionsPerTargetHour: 5}, NewRateLimiter(), podReq)
	if resp.Decision != DecisionPropose {
		t.Fatalf("decision %s: %s", resp.Decision, resp.Reason)
	}
	if resp.Action.Params["pod"] != "crashy-abc" || resp.Action.Params["namespace"] != "demo" {
		t.Fatalf("params not rendered: %v", resp.Action.Params)
	}
	if p := resp.Precheck[0].Params; p["pod"] != "crashy-abc" || p["namespace"] != "demo" {
		t.Fatalf("precheck must inherit the action params: %v", p)
	}
}

func TestAutofixRiskAndLimits(t *testing.T) {
	cat := testCatalog(t)
	pol := Policy{Mode: ModeAutofix, MaxRisk: RiskLow, MaxActionsPerTargetHour: 5}
	rl := NewRateLimiter()

	if d := Evaluate(cat, pol, rl, podReq).Decision; d != DecisionExecute {
		t.Fatalf("low risk: %s", d)
	}

	scale := EvalRequest{Playbook: "scale-up", Target: Target{Kind: "k8s.deployment", Namespace: "demo", Workload: "api"}}
	if d := Evaluate(cat, pol, rl, scale).Decision; d != DecisionApproval {
		t.Fatalf("medium above max: %s", d)
	}
	resp := Evaluate(cat, pol, rl, scale)
	if resp.Rollback == nil || resp.Rollback.Params["delta"] != -1 {
		t.Fatalf("rollback not rendered: %+v", resp.Rollback)
	}

	// Playbook limit (2) is stricter than the global one (5).
	rl.Record(podReq.Target.Key())
	rl.Record(podReq.Target.Key())
	if d := Evaluate(cat, pol, rl, podReq).Decision; d != DecisionApproval {
		t.Fatalf("rate limited: %s", d)
	}
}

func TestDenyCases(t *testing.T) {
	cat := testCatalog(t)
	pol := Policy{Mode: ModeAutofix, MaxRisk: RiskMedium, MaxActionsPerTargetHour: 5}
	cases := map[string]EvalRequest{
		"unknown playbook": {Playbook: "nope"},
		"wrong alert":      {Playbook: podReq.Playbook, Target: podReq.Target, Alert: AlertContext{Name: "Other"}},
		"missing value":    {Playbook: podReq.Playbook, Target: Target{Kind: "k8s.pod", Namespace: "demo"}, Alert: podReq.Alert},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if d := Evaluate(cat, pol, NewRateLimiter(), req).Decision; d != DecisionDeny {
				t.Fatalf("decision %s", d)
			}
		})
	}
}

// The community edition runs only low-risk playbooks without approval, whatever
// AUTOFIX_MAX_RISK says; another Decider (enterprise) can allow more.
func TestCommunityCapsAutofixAtLow(t *testing.T) {
	cat := testCatalog(t)
	scale := EvalRequest{Playbook: "scale-up", Target: Target{Kind: "k8s.deployment", Namespace: "demo", Workload: "api"}}
	pol := Policy{Mode: ModeAutofix, MaxRisk: RiskMedium, MaxActionsPerTargetHour: 5}
	resp := Evaluate(cat, pol, NewRateLimiter(), scale)
	if resp.Decision != DecisionApproval || !strings.Contains(resp.Reason, "enterprise") {
		t.Fatalf("medium in community: %s (%s)", resp.Decision, resp.Reason)
	}
	pol.Decider = allowUpTo{RiskMedium}
	if d := Evaluate(cat, pol, NewRateLimiter(), scale).Decision; d != DecisionExecute {
		t.Fatalf("medium with a custom decider: %s", d)
	}
}

type allowUpTo struct{ max Risk }

func (a allowUpTo) MaxAutoRisk(pol Policy) Risk { return min(pol.MaxRisk, a.max) }

func (a allowUpTo) Decide(pb *Playbook, pol Policy, rl Limiter, req EvalRequest) (Decision, string) {
	if pol.Mode == ModeReadonly || pb.Risk() > a.MaxAutoRisk(pol) {
		return DecisionApproval, "test"
	}
	return RateLimited(pb, pol, rl, req, a.MaxAutoRisk(pol))
}

package guard

// Primitives are the only operations the system can perform. They live in
// code and are reviewed; playbooks only choose and parameterize them.

type ParamType string

const (
	ParamString ParamType = "string"
	ParamInt    ParamType = "int"
)

type ParamSpec struct {
	Type     ParamType
	Required bool
}

type Primitive struct {
	Name        string
	MinRisk     Risk
	TargetKinds []string
	Params      map[string]ParamSpec
	// CheckOnly primitives are read-only conditions usable in precheck.
	CheckOnly bool
}

var podKinds = []string{"k8s.pod"}
var deploymentKinds = []string{"k8s.deployment", "k8s.pod"}

var primitives = map[string]Primitive{}

func register(p Primitive) {
	primitives[p.Name] = p
}

func init() {
	register(Primitive{
		Name: "k8s.pod.delete", MinRisk: RiskLow, TargetKinds: podKinds,
		Params: map[string]ParamSpec{
			"namespace": {ParamString, true},
			"pod":       {ParamString, true},
		},
	})
	register(Primitive{
		Name: "k8s.pod.ownerExists", CheckOnly: true, TargetKinds: podKinds,
		Params: map[string]ParamSpec{
			"namespace": {ParamString, false},
			"pod":       {ParamString, false},
		},
	})
	register(Primitive{
		Name: "k8s.job.delete", MinRisk: RiskLow, TargetKinds: []string{"k8s.job", "k8s.pod"},
		Params: map[string]ParamSpec{
			"namespace": {ParamString, true},
			"job":       {ParamString, true},
		},
	})
	register(Primitive{
		Name: "k8s.deployment.restart", MinRisk: RiskLow, TargetKinds: deploymentKinds,
		Params: map[string]ParamSpec{
			"namespace":  {ParamString, true},
			"deployment": {ParamString, true},
		},
	})
	register(Primitive{
		Name: "k8s.deployment.scale", MinRisk: RiskMedium, TargetKinds: deploymentKinds,
		Params: map[string]ParamSpec{
			"namespace":   {ParamString, true},
			"deployment":  {ParamString, true},
			"delta":       {ParamInt, true},
			"maxReplicas": {ParamInt, false},
		},
	})
	register(Primitive{
		Name: "k8s.deployment.noActiveHPA", CheckOnly: true, TargetKinds: deploymentKinds,
		Params: map[string]ParamSpec{
			"namespace":  {ParamString, false},
			"deployment": {ParamString, false},
		},
	})
	register(Primitive{
		Name: "k8s.node.cordon", MinRisk: RiskMedium, TargetKinds: []string{"k8s.node"},
		Params: map[string]ParamSpec{"node": {ParamString, true}},
	})
	register(Primitive{
		Name: "k8s.node.drain", MinRisk: RiskHigh, TargetKinds: []string{"k8s.node"},
		Params: map[string]ParamSpec{"node": {ParamString, true}},
	})
	register(Primitive{
		Name: "k8s.rollout.undo", MinRisk: RiskHigh, TargetKinds: deploymentKinds,
		Params: map[string]ParamSpec{
			"namespace":  {ParamString, true},
			"deployment": {ParamString, true},
		},
	})
	register(Primitive{
		Name: "host.systemd.restart", MinRisk: RiskMedium, TargetKinds: []string{"host", "host.unit"},
		Params: map[string]ParamSpec{
			"host": {ParamString, true},
			"unit": {ParamString, true},
		},
	})
	register(Primitive{
		Name: "host.path.clean", MinRisk: RiskLow, TargetKinds: []string{"host"},
		Params: map[string]ParamSpec{
			"host": {ParamString, true},
			"path": {ParamString, true},
		},
	})
}

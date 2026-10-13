package guard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const playbookAPIVersion = "ai-sre.io/v1"

type Step struct {
	Primitive string         `yaml:"primitive" json:"primitive"`
	Params    map[string]any `yaml:"params" json:"params,omitempty"`
}

// Rollback is either the scalar "none" or a step.
type Rollback struct {
	None bool
	Step *Step
}

func (r *Rollback) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		if node.Value != "none" {
			return fmt.Errorf("rollback: want \"none\" or a step, got %q", node.Value)
		}
		r.None = true
		return nil
	}
	var s Step
	if err := node.Decode(&s); err != nil {
		return err
	}
	r.Step = &s
	return nil
}

type Match struct {
	Alertnames []string            `yaml:"alertnames" json:"alertnames,omitempty"`
	TargetKind string              `yaml:"targetKind" json:"target_kind,omitempty"`
	Labels     map[string][]string `yaml:"labels" json:"labels,omitempty"`
}

type Verify struct {
	PromQL  string `yaml:"promql" json:"promql"`
	Timeout string `yaml:"timeout" json:"timeout"`
}

type Limits struct {
	MaxPerTargetPerHour int `yaml:"maxPerTargetPerHour" json:"max_per_target_per_hour"`
}

type PlaybookSpec struct {
	Enabled     *bool    `yaml:"enabled"`
	Description string   `yaml:"description"`
	Risk        string   `yaml:"risk"`
	Match       Match    `yaml:"match"`
	Action      Step     `yaml:"action"`
	Precheck    []Step   `yaml:"precheck"`
	Verify      *Verify  `yaml:"verify"`
	Rollback    Rollback `yaml:"rollback"`
	Limits      Limits   `yaml:"limits"`
}

type Playbook struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec PlaybookSpec `yaml:"spec"`

	risk   Risk
	source string
}

func (p *Playbook) Name() string  { return p.Metadata.Name }
func (p *Playbook) Risk() Risk    { return p.risk }
func (p *Playbook) Enabled() bool { return p.Spec.Enabled == nil || *p.Spec.Enabled }

type Catalog struct {
	Version   string
	Playbooks map[string]*Playbook
	LoadedAt  time.Time
}

func (c *Catalog) Sorted() []*Playbook {
	out := make([]*Playbook, 0, len(c.Playbooks))
	for _, p := range c.Playbooks {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// catalogFiles returns the YAML files of a catalog directory, sorted.
// Only the top level is read: a ConfigMap mount keeps its real files in
// hidden ..data directories and exposes them as top-level symlinks.
func catalogFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	sort.Strings(files)
	return files, nil
}

// CatalogVersion hashes file names and contents, so any edit changes it.
func CatalogVersion(dir string) (string, error) {
	files, err := catalogFiles(dir)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		h.Write([]byte(filepath.Base(f)))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// LoadCatalog parses and validates the whole catalog. Any error rejects it.
func LoadCatalog(dir string) (*Catalog, error) {
	version, err := CatalogVersion(dir)
	if err != nil {
		return nil, err
	}
	files, err := catalogFiles(dir)
	if err != nil {
		return nil, err
	}
	cat := &Catalog{Version: version, Playbooks: map[string]*Playbook{}, LoadedAt: time.Now()}
	var errs []error
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		for i := 0; ; i++ {
			var pb Playbook
			err := dec.Decode(&pb)
			if errors.Is(err, io.EOF) {
				break
			}
			src := fmt.Sprintf("%s#%d", filepath.Base(f), i)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", src, err))
				break
			}
			pb.source = src
			if err := validatePlaybook(&pb); err != nil {
				errs = append(errs, fmt.Errorf("%s (%s): %w", src, pb.Name(), err))
				continue
			}
			if prev, ok := cat.Playbooks[pb.Name()]; ok {
				errs = append(errs, fmt.Errorf("%s: duplicate playbook %q, first defined in %s", src, pb.Name(), prev.source))
				continue
			}
			cat.Playbooks[pb.Name()] = &pb
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cat, nil
}

var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func validatePlaybook(pb *Playbook) error {
	if pb.APIVersion != playbookAPIVersion {
		return fmt.Errorf("apiVersion must be %s", playbookAPIVersion)
	}
	if pb.Kind != "Playbook" {
		return fmt.Errorf("kind must be Playbook")
	}
	if !namePattern.MatchString(pb.Name()) {
		return fmt.Errorf("metadata.name %q must be a lowercase DNS label", pb.Name())
	}
	risk, err := ParseRisk(pb.Spec.Risk)
	if err != nil {
		return err
	}
	pb.risk = risk

	action, ok := primitives[pb.Spec.Action.Primitive]
	if !ok {
		return fmt.Errorf("action: unknown primitive %q", pb.Spec.Action.Primitive)
	}
	if action.CheckOnly {
		return fmt.Errorf("action: %s is a check, not an action", action.Name)
	}
	if risk < action.MinRisk {
		return fmt.Errorf("risk %s is below the minimum %s of %s", risk, action.MinRisk, action.Name)
	}
	if pb.Spec.Match.TargetKind != "" && !contains(action.TargetKinds, pb.Spec.Match.TargetKind) {
		return fmt.Errorf("action %s does not apply to target kind %s", action.Name, pb.Spec.Match.TargetKind)
	}
	if err := validateStep("action", pb.Spec.Action, false); err != nil {
		return err
	}
	for i, s := range pb.Spec.Precheck {
		if err := validateStep(fmt.Sprintf("precheck[%d]", i), s, true); err != nil {
			return err
		}
	}
	if pb.Spec.Rollback.Step != nil {
		if err := validateStep("rollback", *pb.Spec.Rollback.Step, false); err != nil {
			return err
		}
	} else if !pb.Spec.Rollback.None {
		return fmt.Errorf("rollback is required: a step or \"none\"")
	}
	if v := pb.Spec.Verify; v != nil {
		if v.PromQL == "" {
			return fmt.Errorf("verify.promql is required")
		}
		if err := checkTemplate(v.PromQL); err != nil {
			return fmt.Errorf("verify.promql: %w", err)
		}
		if _, err := time.ParseDuration(v.Timeout); err != nil {
			return fmt.Errorf("verify.timeout: %w", err)
		}
	}
	if pb.Spec.Limits.MaxPerTargetPerHour < 0 {
		return fmt.Errorf("limits.maxPerTargetPerHour must not be negative")
	}
	return nil
}

func validateStep(where string, s Step, check bool) error {
	p, ok := primitives[s.Primitive]
	if !ok {
		return fmt.Errorf("%s: unknown primitive %q", where, s.Primitive)
	}
	if check && !p.CheckOnly {
		return fmt.Errorf("%s: %s is an action, prechecks must be checks", where, s.Primitive)
	}
	if !check && p.CheckOnly {
		return fmt.Errorf("%s: %s is a check, not an action", where, s.Primitive)
	}
	for name, spec := range p.Params {
		if _, ok := s.Params[name]; spec.Required && !ok {
			return fmt.Errorf("%s: %s requires param %q", where, s.Primitive, name)
		}
	}
	for name, value := range s.Params {
		spec, ok := p.Params[name]
		if !ok {
			return fmt.Errorf("%s: %s has no param %q", where, s.Primitive, name)
		}
		switch v := value.(type) {
		case string:
			if err := checkTemplate(v); err != nil {
				return fmt.Errorf("%s.params.%s: %w", where, name, err)
			}
		case int:
			if spec.Type != ParamInt {
				return fmt.Errorf("%s.params.%s: want %s", where, name, spec.Type)
			}
		default:
			return fmt.Errorf("%s.params.%s: unsupported value type %T", where, name, value)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

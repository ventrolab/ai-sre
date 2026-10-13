package guard

import (
	"fmt"
	"regexp"
	"strings"
)

// Templates support variable substitution only: no expressions, no code.
var templateVar = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.\-]+)\s*\}\}`)

var targetFields = map[string]bool{
	"kind": true, "cluster": true, "namespace": true, "workload": true,
	"pod": true, "node": true, "host": true,
}

func checkVariable(name string) error {
	switch {
	case strings.HasPrefix(name, "target.labels."), strings.HasPrefix(name, "alert.labels."):
		return nil
	case strings.HasPrefix(name, "target."):
		if targetFields[strings.TrimPrefix(name, "target.")] {
			return nil
		}
	}
	return fmt.Errorf("unknown template variable %q", name)
}

func checkTemplate(s string) error {
	rest := templateVar.ReplaceAllString(s, "")
	if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		return fmt.Errorf("malformed template %q", s)
	}
	for _, m := range templateVar.FindAllStringSubmatch(s, -1) {
		if err := checkVariable(m[1]); err != nil {
			return err
		}
	}
	return nil
}

type TemplateContext struct {
	Target Target
	Alert  AlertContext
}

func (c TemplateContext) lookup(name string) (string, bool) {
	t := c.Target
	var v string
	switch {
	case strings.HasPrefix(name, "target.labels."):
		v = t.Labels[strings.TrimPrefix(name, "target.labels.")]
	case strings.HasPrefix(name, "alert.labels."):
		v = c.Alert.Labels[strings.TrimPrefix(name, "alert.labels.")]
	default:
		v = map[string]string{
			"target.kind": t.Kind, "target.cluster": t.Cluster, "target.namespace": t.Namespace,
			"target.workload": t.Workload, "target.pod": t.Pod, "target.node": t.Node, "target.host": t.Host,
		}[name]
	}
	return v, v != ""
}

// Render substitutes variables; a missing value is an error, never an empty string.
func (c TemplateContext) Render(s string) (string, error) {
	var missing []string
	out := templateVar.ReplaceAllStringFunc(s, func(m string) string {
		name := templateVar.FindStringSubmatch(m)[1]
		v, ok := c.lookup(name)
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("no value for %s", strings.Join(missing, ", "))
	}
	return out, nil
}

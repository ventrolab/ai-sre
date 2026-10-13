package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/ventrolab/ai-sre/pkg/agentkit"
)

// Actions are the write primitives of the Action Guard. They are served as
// "action:<primitive>" and "check:<primitive>" tools, which the hub never
// exposes to the investigator.
type Actions struct {
	cs kubernetes.Interface
}

type actionArgs struct {
	Params map[string]any `json:"params"`
	DryRun bool           `json:"dry_run"`
}

func (a actionArgs) str(name string) (string, error) {
	v, _ := a.Params[name].(string)
	if v == "" {
		return "", fmt.Errorf("param %s is required", name)
	}
	return v, nil
}

func (a actionArgs) num(name string, def int) int {
	switch v := a.Params[name].(type) {
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func dryRun(d bool) []string {
	if d {
		return []string{metav1.DryRunAll}
	}
	return nil
}

func prefix(d bool) string {
	if d {
		return "dry run: "
	}
	return ""
}

type action func(ctx context.Context, a actionArgs) (string, error)

func wrap(fn action) agentkit.Tool {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var a actionArgs
		if err := agentkit.Decode(raw, &a); err != nil {
			return "", err
		}
		return fn(ctx, a)
	}
}

func (x *Actions) Toolset() map[string]agentkit.Tool {
	return map[string]agentkit.Tool{
		"action:k8s.pod.delete":            wrap(x.podDelete),
		"action:k8s.job.delete":            wrap(x.jobDelete),
		"action:k8s.deployment.restart":    wrap(x.deploymentRestart),
		"action:k8s.deployment.scale":      wrap(x.deploymentScale),
		"action:k8s.node.cordon":           wrap(x.nodeCordon),
		"action:k8s.rollout.undo":          wrap(x.rolloutUndo),
		"check:k8s.pod.ownerExists":        wrap(x.podOwnerExists),
		"check:k8s.deployment.noActiveHPA": wrap(x.noActiveHPA),
	}
}

func (x *Actions) podDelete(ctx context.Context, a actionArgs) (string, error) {
	ns, err := a.str("namespace")
	if err != nil {
		return "", err
	}
	pod, err := a.str("pod")
	if err != nil {
		return "", err
	}
	err = x.cs.CoreV1().Pods(ns).Delete(ctx, pod, metav1.DeleteOptions{DryRun: dryRun(a.DryRun)})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%sdeleted pod %s/%s", prefix(a.DryRun), ns, pod), nil
}

func (x *Actions) jobDelete(ctx context.Context, a actionArgs) (string, error) {
	ns, err := a.str("namespace")
	if err != nil {
		return "", err
	}
	job, err := a.str("job")
	if err != nil {
		return "", err
	}
	bg := metav1.DeletePropagationBackground
	err = x.cs.BatchV1().Jobs(ns).Delete(ctx, job, metav1.DeleteOptions{DryRun: dryRun(a.DryRun), PropagationPolicy: &bg})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%sdeleted job %s/%s", prefix(a.DryRun), ns, job), nil
}

func (x *Actions) deploymentRestart(ctx context.Context, a actionArgs) (string, error) {
	ns, err := a.str("namespace")
	if err != nil {
		return "", err
	}
	name, err := a.str("deployment")
	if err != nil {
		return "", err
	}
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().UTC().Format(time.RFC3339))
	_, err = x.cs.AppsV1().Deployments(ns).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch),
		metav1.PatchOptions{DryRun: dryRun(a.DryRun)})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%srolling restart of deployment %s/%s", prefix(a.DryRun), ns, name), nil
}

func (x *Actions) deploymentScale(ctx context.Context, a actionArgs) (string, error) {
	ns, err := a.str("namespace")
	if err != nil {
		return "", err
	}
	name, err := a.str("deployment")
	if err != nil {
		return "", err
	}
	delta := a.num("delta", 0)
	if delta == 0 {
		return "", fmt.Errorf("param delta must not be 0")
	}
	maxReplicas := a.num("maxReplicas", 10)
	scale, err := x.cs.AppsV1().Deployments(ns).GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	cur := scale.Spec.Replicas
	next := cur + int32(delta)
	if next < 1 {
		return "", fmt.Errorf("refusing to scale %s/%s below 1 replica (now %d)", ns, name, cur)
	}
	if next > int32(maxReplicas) {
		return "", fmt.Errorf("refusing to scale %s/%s above maxReplicas=%d (now %d)", ns, name, maxReplicas, cur)
	}
	scale.Spec.Replicas = next
	if _, err := x.cs.AppsV1().Deployments(ns).UpdateScale(ctx, name, scale,
		metav1.UpdateOptions{DryRun: dryRun(a.DryRun)}); err != nil {
		return "", err
	}
	return fmt.Sprintf("%sscaled deployment %s/%s from %d to %d replicas", prefix(a.DryRun), ns, name, cur, next), nil
}

func (x *Actions) nodeCordon(ctx context.Context, a actionArgs) (string, error) {
	node, err := a.str("node")
	if err != nil {
		return "", err
	}
	_, err = x.cs.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType, []byte(`{"spec":{"unschedulable":true}}`),
		metav1.PatchOptions{DryRun: dryRun(a.DryRun)})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%scordoned node %s", prefix(a.DryRun), node), nil
}

// rolloutUndo restores the pod template of the previous revision, like
// kubectl rollout undo.
func (x *Actions) rolloutUndo(ctx context.Context, a actionArgs) (string, error) {
	ns, err := a.str("namespace")
	if err != nil {
		return "", err
	}
	name, err := a.str("deployment")
	if err != nil {
		return "", err
	}
	dep, err := x.cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return "", err
	}
	rsList, err := x.cs.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return "", err
	}
	type rev struct {
		n  int
		rs appsv1.ReplicaSet
	}
	var revs []rev
	for _, rs := range rsList.Items {
		if !metav1.IsControlledBy(&rs, dep) {
			continue
		}
		n, _ := strconv.Atoi(rs.Annotations["deployment.kubernetes.io/revision"])
		revs = append(revs, rev{n, rs})
	}
	sort.Slice(revs, func(i, j int) bool { return revs[i].n > revs[j].n })
	if len(revs) < 2 {
		return "", fmt.Errorf("deployment %s/%s has no previous revision", ns, name)
	}
	prev := revs[1]
	tmpl := prev.rs.Spec.Template.DeepCopy()
	delete(tmpl.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": tmpl}})
	if _, err := x.cs.AppsV1().Deployments(ns).Patch(ctx, name, types.StrategicMergePatchType, patch,
		metav1.PatchOptions{DryRun: dryRun(a.DryRun)}); err != nil {
		return "", err
	}
	return fmt.Sprintf("%srolled back deployment %s/%s from revision %d to %d", prefix(a.DryRun), ns, name, revs[0].n, prev.n), nil
}

func (x *Actions) podOwnerExists(ctx context.Context, a actionArgs) (string, error) {
	ns, err := a.str("namespace")
	if err != nil {
		return "", err
	}
	pod, err := a.str("pod")
	if err != nil {
		return "", err
	}
	p, err := x.cs.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if ref := metav1.GetControllerOf(p); ref != nil {
		return fmt.Sprintf("pod %s/%s is controlled by %s %s", ns, pod, ref.Kind, ref.Name), nil
	}
	return "", fmt.Errorf("pod %s/%s has no controller: deleting it would lose it", ns, pod)
}

func (x *Actions) noActiveHPA(ctx context.Context, a actionArgs) (string, error) {
	ns, err := a.str("namespace")
	if err != nil {
		return "", err
	}
	name, err := a.str("deployment")
	if err != nil {
		return "", err
	}
	hpas, err := x.cs.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for _, h := range hpas.Items {
		if h.Spec.ScaleTargetRef.Kind == "Deployment" && h.Spec.ScaleTargetRef.Name == name {
			return "", fmt.Errorf("HPA %s manages deployment %s/%s; manual scaling would be overridden", h.Name, ns, name)
		}
	}
	return fmt.Sprintf("no HPA manages deployment %s/%s", ns, name), nil
}

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func call(t *testing.T, fn func(context.Context, json.RawMessage) (string, error), args string) (string, error) {
	t.Helper()
	return fn(context.Background(), json.RawMessage(args))
}

func TestPodActions(t *testing.T) {
	ctrl := true
	owned := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "demo",
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc", Controller: &ctrl}}}}
	bare := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "demo"}}
	cs := fake.NewSimpleClientset(owned, bare)
	tools := (&Actions{cs: cs}).Toolset()

	if _, err := call(t, tools["check:k8s.pod.ownerExists"], `{"params":{"namespace":"demo","pod":"bare"}}`); err == nil {
		t.Error("bare pod must fail the owner check")
	}
	if out, err := call(t, tools["check:k8s.pod.ownerExists"], `{"params":{"namespace":"demo","pod":"web-1"}}`); err != nil || !strings.Contains(out, "ReplicaSet") {
		t.Fatalf("owner check: %q %v", out, err)
	}
	out, err := call(t, tools["action:k8s.pod.delete"], `{"params":{"namespace":"demo","pod":"web-1"},"dry_run":true}`)
	if err != nil || !strings.HasPrefix(out, "dry run") {
		t.Fatalf("dry run: %q %v", out, err)
	}
	var sawDryRun bool
	for _, a := range cs.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok && len(d.GetDeleteOptions().DryRun) == 1 {
			sawDryRun = true
		}
	}
	if !sawDryRun {
		t.Error("dry run must be passed to the API server")
	}
	if _, err := call(t, tools["action:k8s.pod.delete"], `{"params":{"namespace":"demo"}}`); err == nil {
		t.Error("missing pod param must fail")
	}
}

func TestScaleBounds(t *testing.T) {
	r := int32(2)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo"}, Spec: appsv1.DeploymentSpec{Replicas: &r}}
	cs := fake.NewSimpleClientset(dep)
	// The fake clientset has no scale subresource; answer it from the deployment.
	cs.PrependReactor("get", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "scale" {
			return false, nil, nil
		}
		return true, &autoscalingv1.Scale{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo"},
			Spec: autoscalingv1.ScaleSpec{Replicas: r}}, nil
	})
	cs.PrependReactor("update", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return a.GetSubresource() == "scale", a.(k8stesting.UpdateAction).GetObject(), nil
	})
	tools := (&Actions{cs: cs}).Toolset()
	if out, err := call(t, tools["action:k8s.deployment.scale"], `{"params":{"namespace":"demo","deployment":"web","delta":1}}`); err != nil || !strings.Contains(out, "from 2 to 3") {
		t.Fatalf("scale: %q %v", out, err)
	}
	if _, err := call(t, tools["action:k8s.deployment.scale"], `{"params":{"namespace":"demo","deployment":"web","delta":9,"maxReplicas":5}}`); err == nil {
		t.Error("scale above maxReplicas must be refused")
	}
	if _, err := call(t, tools["action:k8s.deployment.scale"], `{"params":{"namespace":"demo","deployment":"web","delta":-5}}`); err == nil {
		t.Error("scale below 1 must be refused")
	}
}

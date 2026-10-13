package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"

	"github.com/ventrolab/ai-sre/pkg/agentkit"
)

// Kinds whose content the agent never returns.
var forbidden = map[string]bool{"secrets": true}

type K8s struct {
	cfg    *rest.Config
	http   *http.Client
	cs     kubernetes.Interface
	mapper *restmapper.DeferredDiscoveryRESTMapper
}

func NewK8s(cfg *rest.Config) (*K8s, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	return &K8s{cfg: cfg, http: hc, cs: cs,
		mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))}, nil
}

// mapping resolves "pods", "Pod", "deploy", "ingresses.networking.k8s.io".
func (k *K8s) mapping(kind string) (*meta.RESTMapping, error) {
	kind = strings.TrimSpace(kind)
	resource, group, _ := strings.Cut(strings.ToLower(kind), ".")
	try := func() (*meta.RESTMapping, error) {
		gvr, err := k.mapper.ResourceFor(schema.GroupVersionResource{Group: group, Resource: resource})
		if err != nil {
			gvk, kerr := k.mapper.KindFor(schema.GroupVersionResource{Group: group, Resource: resource})
			if kerr != nil {
				return nil, fmt.Errorf("unknown kind %q", kind)
			}
			return k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		}
		gvk, err := k.mapper.KindFor(gvr)
		if err != nil {
			return nil, err
		}
		return k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	m, err := try()
	if err != nil {
		k.mapper.Reset() // new CRDs since the last discovery
		m, err = try()
	}
	if err != nil {
		return nil, err
	}
	if forbidden[m.Resource.Resource] {
		return nil, fmt.Errorf("%s are not available to the investigation", m.Resource.Resource)
	}
	return m, nil
}

func (k *K8s) path(m *meta.RESTMapping, namespace, name string) string {
	var p string
	if m.Resource.Group == "" {
		p = "/api/" + m.Resource.Version
	} else {
		p = "/apis/" + m.Resource.Group + "/" + m.Resource.Version
	}
	if m.Scope.Name() == meta.RESTScopeNameNamespace && namespace != "" {
		p += "/namespaces/" + url.PathEscape(namespace)
	}
	p += "/" + m.Resource.Resource
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

func (k *K8s) get(ctx context.Context, path string, query url.Values, accept string) ([]byte, error) {
	u := strings.TrimRight(k.cfg.Host, "/") + k.cfg.APIPath + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		var st metav1.Status
		if json.Unmarshal(body, &st) == nil && st.Message != "" {
			return nil, fmt.Errorf("%s", st.Message)
		}
		return nil, fmt.Errorf("API returned %s", resp.Status)
	}
	return body, nil
}

const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io,application/json"

type listArgs struct {
	Kind          string `json:"kind"`
	Namespace     string `json:"namespace"`
	LabelSelector string `json:"label_selector"`
	FieldSelector string `json:"field_selector"`
	Limit         int    `json:"limit"`
}

func (k *K8s) List(ctx context.Context, raw json.RawMessage) (string, error) {
	var a listArgs
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	m, err := k.mapping(a.Kind)
	if err != nil {
		return "", err
	}
	q := url.Values{"includeObject": {"Metadata"}}
	q.Set("limit", fmt.Sprint(agentkit.Clamp(a.Limit, 200, 1000)))
	if a.LabelSelector != "" {
		q.Set("labelSelector", a.LabelSelector)
	}
	if a.FieldSelector != "" {
		q.Set("fieldSelector", a.FieldSelector)
	}
	body, err := k.get(ctx, k.path(m, a.Namespace, ""), q, tableAccept)
	if err != nil {
		return "", err
	}
	allNS := m.Scope.Name() == meta.RESTScopeNameNamespace && a.Namespace == ""
	return renderTable(body, allNS)
}

func renderTable(body []byte, withNamespace bool) (string, error) {
	var t metav1.Table
	if err := json.Unmarshal(body, &t); err != nil {
		return "", fmt.Errorf("unexpected API response: %w", err)
	}
	if len(t.Rows) == 0 {
		return "No objects found.", nil
	}
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	var cols []int
	var header []string
	if withNamespace {
		header = append(header, "NAMESPACE")
	}
	for i, c := range t.ColumnDefinitions {
		if c.Priority == 0 {
			cols = append(cols, i)
			header = append(header, strings.ToUpper(c.Name))
		}
	}
	fmt.Fprintln(w, strings.Join(header, "\t"))
	for _, row := range t.Rows {
		var cells []string
		if withNamespace {
			var obj metav1.PartialObjectMetadata
			_ = json.Unmarshal(row.Object.Raw, &obj)
			cells = append(cells, obj.Namespace)
		}
		for _, i := range cols {
			if i < len(row.Cells) {
				cells = append(cells, fmt.Sprint(row.Cells[i]))
			}
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	w.Flush()
	if t.Continue != "" {
		buf.WriteString("... more objects exist; narrow the selector\n")
	}
	return buf.String(), nil
}

type getArgs struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (k *K8s) Get(ctx context.Context, raw json.RawMessage) (string, error) {
	var a getArgs
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	m, err := k.mapping(a.Kind)
	if err != nil {
		return "", err
	}
	if m.Scope.Name() == meta.RESTScopeNameNamespace && a.Namespace == "" {
		return "", fmt.Errorf("%s are namespaced: pass namespace", m.Resource.Resource)
	}
	body, err := k.get(ctx, k.path(m, a.Namespace, a.Name), nil, "application/json")
	if err != nil {
		return "", err
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return "", err
	}
	if md, ok := obj["metadata"].(map[string]any); ok {
		delete(md, "managedFields")
		if ann, ok := md["annotations"].(map[string]any); ok {
			delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
		}
	}
	out, err := yaml.Marshal(obj)
	return string(out), err
}

type eventsArgs struct {
	Namespace string `json:"namespace"`
	Object    string `json:"object"`
	Limit     int    `json:"limit"`
}

func (k *K8s) Events(ctx context.Context, raw json.RawMessage) (string, error) {
	var a eventsArgs
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	opts := metav1.ListOptions{}
	if a.Object != "" {
		opts.FieldSelector = "involvedObject.name=" + a.Object
	}
	list, err := k.cs.CoreV1().Events(a.Namespace).List(ctx, opts)
	if err != nil {
		return "", err
	}
	items := list.Items
	when := func(e corev1.Event) time.Time {
		for _, t := range []time.Time{e.LastTimestamp.Time, e.EventTime.Time, e.FirstTimestamp.Time, e.CreationTimestamp.Time} {
			if !t.IsZero() {
				return t
			}
		}
		return time.Time{}
	}
	sort.Slice(items, func(i, j int) bool { return when(items[i]).Before(when(items[j])) })
	limit := agentkit.Clamp(a.Limit, 100, 500)
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	if len(items) == 0 {
		return "No events.", nil
	}
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LAST SEEN\tTYPE\tREASON\tOBJECT\tCOUNT\tMESSAGE")
	for _, e := range items {
		obj := strings.ToLower(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Name
		if a.Namespace == "" {
			obj = e.Namespace + "/" + obj
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", when(e).UTC().Format(time.RFC3339), e.Type, e.Reason, obj,
			max(e.Count, 1), strings.ReplaceAll(e.Message, "\n", " "))
	}
	w.Flush()
	return buf.String(), nil
}

type logsArgs struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Previous  bool   `json:"previous"`
	Tail      int    `json:"tail"`
	Since     string `json:"since"`
	Grep      string `json:"grep"`
}

func (k *K8s) Logs(ctx context.Context, raw json.RawMessage) (string, error) {
	var a logsArgs
	if err := agentkit.Decode(raw, &a); err != nil {
		return "", err
	}
	pod, err := k.cs.CoreV1().Pods(a.Namespace).Get(ctx, a.Pod, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	container := a.Container
	if container == "" {
		if len(pod.Spec.Containers) > 1 {
			names := make([]string, len(pod.Spec.Containers))
			for i, c := range pod.Spec.Containers {
				names[i] = c.Name
			}
			return "", fmt.Errorf("pod has several containers, pass container: %s", strings.Join(names, ", "))
		}
		container = pod.Spec.Containers[0].Name
	}
	tail := int64(agentkit.Clamp(a.Tail, 200, 2000))
	opts := &corev1.PodLogOptions{Container: container, Previous: a.Previous, TailLines: &tail, Timestamps: true}
	if a.Since != "" {
		d, err := agentkit.Since(a.Since, time.Hour, 7*24*time.Hour)
		if err != nil {
			return "", err
		}
		secs := int64(d.Seconds())
		opts.SinceSeconds = &secs
	}
	data, err := k.cs.CoreV1().Pods(a.Namespace).GetLogs(a.Pod, opts).DoRaw(ctx)
	if err != nil {
		return "", err
	}
	out := string(data)
	if a.Grep != "" {
		re, err := regexp.Compile("(?i)" + a.Grep)
		if err != nil {
			return "", fmt.Errorf("invalid grep: %w", err)
		}
		var kept []string
		for _, line := range strings.Split(out, "\n") {
			if re.MatchString(line) {
				kept = append(kept, line)
			}
		}
		out = strings.Join(kept, "\n")
	}
	if strings.TrimSpace(out) == "" {
		return "No log lines.", nil
	}
	return out, nil
}

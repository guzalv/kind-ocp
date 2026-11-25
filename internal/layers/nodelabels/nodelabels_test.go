package nodelabels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stackrox/kind-ocp/internal/kube"
	"github.com/stackrox/kind-ocp/layers"
)

func TestLayerName(t *testing.T) {
	l := Layer{}
	if got := l.Name(); got != Name {
		t.Errorf("Name() = %q, want %q", got, Name)
	}
	if Name != "node-labels" {
		t.Errorf("Name = %q, want the documented layer name node-labels", Name)
	}
}

// The apply configuration is a partial Node carrying only the name and
// the label, so the server-side apply claims exactly that
// metadata.labels key and cannot disturb anything else on the node.
// Its marshalled form is the apply request body, so pinning the JSON
// pins what goes on the wire.
func TestMasterLabelApply(t *testing.T) {
	cfg := masterLabelApply("kind-ocp-1-control-plane")
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshalling the apply configuration: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the apply body is not JSON: %v (body: %s)", err, body)
	}
	for _, key := range []string{"spec", "status"} {
		if _, found := doc[key]; found {
			t.Errorf("apply body carries %q (%s); a label patch must stay metadata-only", key, body)
		}
	}
	metadata, ok := doc["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("apply body carries no metadata object (%s)", body)
	}
	if metadata["name"] != "kind-ocp-1-control-plane" {
		t.Errorf("metadata.name = %v, want the target node (%s)", metadata["name"], body)
	}
	labels, ok := metadata["labels"].(map[string]any)
	if !ok || len(labels) != 1 {
		t.Fatalf("metadata.labels = %v, want exactly the master role label (%s)", metadata["labels"], body)
	}
	if got, ok := labels[masterLabel]; !ok || got != "" {
		t.Errorf("metadata.labels[%s] = %v (present: %t), want the empty-string role label", masterLabel, got, ok)
	}
}

func TestCheckMasterLabel(t *testing.T) {
	node := func(name string, labels map[string]string) corev1.Node {
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}

	t.Run("all control-plane nodes labeled passes", func(t *testing.T) {
		nodes := []corev1.Node{
			node("cp1", map[string]string{masterLabel: ""}),
			node("cp2", map[string]string{controlPlaneLabel: "", masterLabel: ""}),
		}
		if err := checkMasterLabel(nodes); err != nil {
			t.Fatalf("checkMasterLabel(labeled) = %v, want nil", err)
		}
	})

	t.Run("unlabeled node fails and is named", func(t *testing.T) {
		nodes := []corev1.Node{
			node("cp1", map[string]string{masterLabel: ""}),
			node("cp2", map[string]string{controlPlaneLabel: ""}),
		}
		err := checkMasterLabel(nodes)
		if err == nil || !strings.Contains(err.Error(), "cp2") || !strings.Contains(err.Error(), "label missing") {
			t.Fatalf("checkMasterLabel(cp2 unlabeled) = %v, want an error naming cp2 as missing the label", err)
		}
	})

	// The regression the increment-3 review found live: a non-empty
	// value matched the old presence-only check while scheduling
	// master-selected Deployments nowhere (their nodeSelectors match
	// master: "" exactly).
	t.Run("non-empty label value fails and is named with its value", func(t *testing.T) {
		nodes := []corev1.Node{
			node("cp1", map[string]string{controlPlaneLabel: "", masterLabel: "weird"}),
		}
		err := checkMasterLabel(nodes)
		if err == nil || !strings.Contains(err.Error(), "cp1") || !strings.Contains(err.Error(), "weird") {
			t.Fatalf("checkMasterLabel(cp1 master=weird) = %v, want an error naming cp1 and its value", err)
		}
	})

	t.Run("no control-plane nodes fails", func(t *testing.T) {
		err := checkMasterLabel(nil)
		if err == nil || !strings.Contains(err.Error(), "no nodes") {
			t.Fatalf("checkMasterLabel(nil) = %v, want a no-nodes error", err)
		}
	})
}

// writeKubeconfig writes a kubeconfig pointing at serverURL, the same
// shape the state store exports.
func writeKubeconfig(t *testing.T, serverURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster:
    server: %s
contexts:
- name: fake
  context:
    cluster: fake
    user: fake
current-context: fake
users:
- name: fake
  user: {}
`, serverURL)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testEnv(kubeconfig string) layers.Env {
	return layers.Env{KubeconfigPath: kubeconfig, Log: slog.New(slog.DiscardHandler)}
}

// nodeList renders the NodeList the fake apiserver serves for the
// control-plane selector.
func nodeList(nodes map[string]map[string]string) string {
	var b strings.Builder
	b.WriteString(`{"kind":"NodeList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[`)
	first := true
	for name, labels := range nodes {
		if !first {
			b.WriteString(",")
		}
		first = false
		fmt.Fprintf(&b, `{"metadata":{"name":%q,"labels":{`, name)
		lfirst := true
		for k, v := range labels {
			if !lfirst {
				b.WriteString(",")
			}
			lfirst = false
			fmt.Fprintf(&b, "%q:%q", k, v)
		}
		b.WriteString("}}}")
	}
	b.WriteString("]}")
	return b.String()
}

// nodeApply records one node apply as seen on the wire: the target,
// the request's content type and query (the field manager and force
// flag that make the apply a kind-ocp server-side apply), and the
// metadata.labels the body carried.
type nodeApply struct {
	node        string
	contentType string
	query       url.Values
	labels      map[string]string
}

// fakeNodeAPIServer serves the node API the layer touches — nothing
// else, and in particular no discovery: the typed client the layer
// applies through resolves Node's fixed resource path without a REST
// mapper, so a request for any other endpoint would fail the tests.
// Applies are merged into the served labels, the way the apiserver
// stores them, so a re-list after an apply observes the converge.
type fakeNodeAPIServer struct {
	server *httptest.Server

	nodes    map[string]map[string]string // name -> labels, as listed
	mu       sync.Mutex
	selector string      // labelSelector the list carried
	applies  []nodeApply // node applies, in order
}

func newFakeNodeAPIServer(t *testing.T, nodes map[string]map[string]string) *fakeNodeAPIServer {
	t.Helper()
	api := &fakeNodeAPIServer{nodes: nodes}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		api.selector = r.URL.Query().Get("labelSelector")
		api.mu.Unlock()
		api.write(w, nodeList(api.nodes))
	})
	mux.HandleFunc("/api/v1/nodes/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var doc struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		api.applies = append(api.applies, nodeApply{
			node:        doc.Metadata.Name,
			contentType: r.Header.Get("Content-Type"),
			query:       r.URL.Query(),
			labels:      doc.Metadata.Labels,
		})
		if api.nodes[doc.Metadata.Name] == nil {
			api.nodes[doc.Metadata.Name] = map[string]string{}
		}
		for k, v := range doc.Metadata.Labels {
			api.nodes[doc.Metadata.Name][k] = v
		}
		api.write(w, nodeList(map[string]map[string]string{doc.Metadata.Name: api.nodes[doc.Metadata.Name]}))
	})

	api.server = httptest.NewServer(mux)
	t.Cleanup(api.server.Close)
	return api
}

// applyNames returns the nodes the layer applied to, in apply order.
func (a *fakeNodeAPIServer) applyNames() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	names := make([]string, 0, len(a.applies))
	for _, apply := range a.applies {
		names = append(names, apply.node)
	}
	return names
}

func (a *fakeNodeAPIServer) write(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func TestApply(t *testing.T) {
	// What the apiserver returns for the control-plane selector: cp1
	// lacks the master label, cp2 already carries the converged value,
	// and cp3 carries a non-empty one — both deviations must converge.
	api := newFakeNodeAPIServer(t, map[string]map[string]string{
		"cp1": {controlPlaneLabel: ""},
		"cp2": {controlPlaneLabel: "", masterLabel: ""},
		"cp3": {controlPlaneLabel: "", masterLabel: "weird"},
	})
	if err := (Layer{}).Apply(context.Background(), testEnv(writeKubeconfig(t, api.server.URL))); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	api.mu.Lock()
	selector := api.selector
	applies := slices.Clone(api.applies)
	api.mu.Unlock()

	if selector != controlPlaneLabel {
		t.Errorf("node list selector = %q, want %q (the PoC's selector)", selector, controlPlaneLabel)
	}
	// The list order follows the map, so compare as a sorted set.
	applied := make([]string, 0, len(applies))
	for _, apply := range applies {
		applied = append(applied, apply.node)
	}
	slices.Sort(applied)
	if !slices.Equal(applied, []string{"cp1", "cp3"}) {
		t.Errorf("applied to %v, want exactly cp1 (missing) and cp3 (non-empty), not cp2 (converged)", applied)
	}
	for _, apply := range applies {
		if apply.contentType != string(types.ApplyYAMLPatchType) {
			t.Errorf("apply to %s content type = %q, want %q (a server-side apply)",
				apply.node, apply.contentType, types.ApplyYAMLPatchType)
		}
		if apply.query.Get("fieldManager") != kube.LayerFieldManager(Name) {
			t.Errorf("apply to %s fieldManager = %q, want %q (the layer's own apply identity)",
				apply.node, apply.query.Get("fieldManager"), kube.LayerFieldManager(Name))
		}
		if apply.query.Get("force") != "true" {
			t.Errorf("apply to %s force = %q, want true (a re-run must re-acquire the field)", apply.node, apply.query.Get("force"))
		}
		if got, ok := apply.labels[masterLabel]; !ok || got != "" {
			t.Errorf("apply to %s labels = %v, want the empty-string master role label", apply.node, apply.labels)
		}
	}
}

func TestApplyAllLabeledIsANoop(t *testing.T) {
	api := newFakeNodeAPIServer(t, map[string]map[string]string{
		"cp1": {controlPlaneLabel: "", masterLabel: ""},
	})
	if err := (Layer{}).Apply(context.Background(), testEnv(writeKubeconfig(t, api.server.URL))); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if names := api.applyNames(); len(names) != 0 {
		t.Errorf("applied to %v, want no applies when every node already carries the label", names)
	}
}

func TestApplyNoControlPlaneNodesFails(t *testing.T) {
	api := newFakeNodeAPIServer(t, map[string]map[string]string{})
	err := (Layer{}).Apply(context.Background(), testEnv(writeKubeconfig(t, api.server.URL)))
	if err == nil || !strings.Contains(err.Error(), "no nodes carry") {
		t.Fatalf("Apply(no control-plane nodes) = %v, want a no-nodes error", err)
	}
}

func TestVerify(t *testing.T) {
	t.Run("every control-plane node labeled passes", func(t *testing.T) {
		api := newFakeNodeAPIServer(t, map[string]map[string]string{
			"cp1": {controlPlaneLabel: "", masterLabel: ""},
		})
		if err := (Layer{}).Verify(context.Background(), testEnv(writeKubeconfig(t, api.server.URL))); err != nil {
			t.Fatalf("Verify = %v, want nil", err)
		}
	})

	t.Run("unlabeled control-plane node fails", func(t *testing.T) {
		api := newFakeNodeAPIServer(t, map[string]map[string]string{
			"cp1": {controlPlaneLabel: ""},
		})
		err := (Layer{}).Verify(context.Background(), testEnv(writeKubeconfig(t, api.server.URL)))
		if err == nil || !strings.Contains(err.Error(), "cp1") {
			t.Fatalf("Verify(cp1 unlabeled) = %v, want an error naming cp1", err)
		}
	})

	t.Run("non-empty master label fails and names the value", func(t *testing.T) {
		api := newFakeNodeAPIServer(t, map[string]map[string]string{
			"cp1": {controlPlaneLabel: "", masterLabel: "weird"},
		})
		err := (Layer{}).Verify(context.Background(), testEnv(writeKubeconfig(t, api.server.URL)))
		if err == nil || !strings.Contains(err.Error(), "cp1") || !strings.Contains(err.Error(), "weird") {
			t.Fatalf("Verify(cp1 master=weird) = %v, want an error naming cp1 and its value", err)
		}
	})
}

// The regression the increment-3 review reproduced on a live cluster: a
// master label with a non-empty value — nothing in kind sets one, but
// a stray `kubectl label node -l node-role.kubernetes.io/control-plane
// node-role.kubernetes.io/master=weird` produces exactly this state —
// passed the old presence-only Verify while scheduling
// master-selected Deployments nowhere. Verify must reject it, and a
// re-run of the layer must converge the value back to the empty
// string.
func TestReconcileRestoresEmptyMasterLabelValue(t *testing.T) {
	api := newFakeNodeAPIServer(t, map[string]map[string]string{
		"cp1": {controlPlaneLabel: "", masterLabel: "weird"},
	})
	env := testEnv(writeKubeconfig(t, api.server.URL))

	if err := (Layer{}).Verify(context.Background(), env); err == nil ||
		!strings.Contains(err.Error(), "cp1") || !strings.Contains(err.Error(), "weird") {
		t.Fatalf("Verify(non-empty label) = %v, want an error naming cp1 and its value", err)
	}

	if err := (Layer{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if names := api.applyNames(); !slices.Equal(names, []string{"cp1"}) {
		t.Fatalf("applied to %v, want exactly the deviating node cp1", names)
	}

	// The fake stores the applied labels, so this re-list observes the
	// converge the same way a live cluster would.
	if err := (Layer{}).Verify(context.Background(), env); err != nil {
		t.Fatalf("Verify after reconcile = %v, want nil (the empty value restored)", err)
	}
}

// The same regression class the increment-2 gates found in the olm
// layer: Verify's read ran under the caller's unbounded context, so a
// wedged apiserver hung `kind-ocp verify` indefinitely (the CLI hands
// it context.Background). The fake apiserver stalls the node list far
// beyond the read budget; the read must fail within the bound.
func TestVerifyReadIsBounded(t *testing.T) {
	const readBudget = 200 * time.Millisecond
	const stall = 3 * time.Second

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(stall):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	err := (Layer{}).verify(context.Background(), testEnv(writeKubeconfig(t, srv.URL)), readBudget)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Verify must fail: the apiserver never answered")
	}
	if !strings.Contains(err.Error(), "listing control-plane nodes") {
		t.Errorf("error = %v, want the bounded read to fail it", err)
	}
	if elapsed > time.Second {
		t.Errorf("%s read budget took %s; the stalled read was not cancelled at the deadline", readBudget, elapsed)
	}
}

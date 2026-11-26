package kindocp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/guzalv/kind-ocp/layers"
	"github.com/guzalv/kind-ocp/provider"
)

// probeCall records one ProbeTCPFromNode invocation.
type probeCall struct {
	cluster string
	addr    netip.Addr
	port    int
}

// tcpProbeProvisioner is a fakeProvisioner that also implements
// provider.TCPProber and provider.ControlPlaneAPIEndpointResolver:
// it records the connects the cross-cluster check requests, can be
// told to fail the connects of one source cluster, can run a hook per
// probe (the cancellation regression cancels the verify context from
// inside one), and answers the endpoint corroboration for each
// cluster from tables the tests plant — the same relationship the
// real provisioner has between the node it connects from, the
// addresses it reports for it, and the port it configured that node's
// apiserver to listen on.
type tcpProbeProvisioner struct {
	*fakeProvisioner
	mu           sync.Mutex
	probes       []probeCall
	failFrom     map[string]bool
	onProbe      func(probeCall) error
	apiEndpoints map[string][]netip.AddrPort
	endpointErr  map[string]error
	resolved     []string // endpoint corroboration lookups, in order
}

func newTCPProbeProvisioner() *tcpProbeProvisioner {
	return &tcpProbeProvisioner{
		fakeProvisioner: newFakeProvisioner(),
		failFrom:        map[string]bool{},
		apiEndpoints:    map[string][]netip.AddrPort{},
		endpointErr:     map[string]error{},
	}
}

func (p *tcpProbeProvisioner) ProbeTCPFromNode(_ context.Context, cluster string, addr netip.Addr, port int) error {
	call := probeCall{cluster: cluster, addr: addr, port: port}
	p.mu.Lock()
	p.probes = append(p.probes, call)
	fail := p.failFrom[cluster]
	hook := p.onProbe
	p.mu.Unlock()
	if hook != nil {
		return hook(call)
	}
	if fail {
		return errors.New("bash: connect: Connection refused")
	}
	return nil
}

// ControlPlaneAPIEndpoints implements
// provider.ControlPlaneAPIEndpointResolver: the endpoints the tests
// planted for a cluster, or an error where they planted one — the
// shape of a runtime that cannot reach Docker for the inspect.
func (p *tcpProbeProvisioner) ControlPlaneAPIEndpoints(_ context.Context, cluster string) ([]netip.AddrPort, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resolved = append(p.resolved, cluster)
	if err := p.endpointErr[cluster]; err != nil {
		return nil, err
	}
	endpoints, ok := p.apiEndpoints[cluster]
	if !ok {
		return nil, fmt.Errorf("no API endpoints planted for cluster %s", cluster)
	}
	return append([]netip.AddrPort(nil), endpoints...), nil
}

func (p *tcpProbeProvisioner) probeList() []probeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]probeCall(nil), p.probes...)
}

func (p *tcpProbeProvisioner) resolutionList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.resolved...)
}

// nodePortClientset builds the fake apiserver one cluster needs for
// verify and the cross-cluster check: a Ready node, kube-system
// carrying the instance ID, the kubernetes service's EndpointSlice at
// nodeIP, and a create-reactor that allocates the node port a real
// apiserver assigns. extra runs against the clientset after the
// defaults are in place, so tests can plant hostile or leftover state.
func nodePortClientset(nodeIP string, nodePort int32, extra func(*fake.Clientset)) *fake.Clientset {
	cs := clientsetFor(testInstanceID, corev1.ConditionTrue)
	_ = cs.Tracker().Add(&discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kubernetes",
			Namespace: "default",
			Labels:    map[string]string{discoveryv1.LabelServiceName: "kubernetes"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{nodeIP}},
		},
		Ports: []discoveryv1.EndpointPort{
			{Name: ptrTo("https"), Protocol: ptrTo(corev1.ProtocolTCP), Port: ptrTo(int32(6443))},
		},
	})
	cs.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create := action.(k8stesting.CreateAction)
		svc := create.GetObject().(*corev1.Service)
		for i := range svc.Spec.Ports {
			svc.Spec.Ports[i].NodePort = nodePort
		}
		// A handled reactor replaces the default one, which is what
		// stores the object; store it here so later reads see it.
		if err := cs.Tracker().Create(create.GetResource(), svc, svc.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, svc, nil
	})
	if extra != nil {
		extra(cs)
	}
	return cs
}

func ptrTo[T any](v T) *T { return &v }

// nodePortTestClient builds a client over count clusters backed by
// tcpProbeProvisioner, whose clientset function hands each cluster its
// own fake apiserver. The per-cluster fakes are returned too, keyed by
// cluster name, so tests can inspect what the check left behind.
func nodePortTestClient(t *testing.T, tweak func(cs *fake.Clientset, cluster string)) (*Client, *tcpProbeProvisioner, map[string]*fake.Clientset) {
	t.Helper()
	cfg := testConfig(t, 2, "minimal")
	prov := newTCPProbeProvisioner()
	byCluster := map[string]*fake.Clientset{}
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	client.clientset = func(path string) (kubernetes.Interface, error) {
		for name, cs := range byCluster {
			if strings.Contains(path, "/"+name+"/") {
				return cs, nil
			}
		}
		return nil, fmt.Errorf("no fake apiserver for kubeconfig %s", path)
	}
	for i := 1; i <= 2; i++ {
		name := fmt.Sprintf("p-%d", i)
		nodeIP := fmt.Sprintf("10.42.0.%d", i)
		var extra func(*fake.Clientset)
		if tweak != nil {
			extra = func(cs *fake.Clientset) { tweak(cs, name) }
		}
		byCluster[name] = nodePortClientset(nodeIP, int32(31000+i), extra)
		// The corroboration sees exactly what the cluster's own
		// EndpointSlice reports — the honest relationship; a test
		// plants a different table (or a different slice) to force
		// the mismatch.
		prov.mu.Lock()
		prov.apiEndpoints[name] = []netip.AddrPort{netip.MustParseAddrPort(nodeIP + ":6443")}
		prov.mu.Unlock()
	}
	return client, prov, byCluster
}

// crossClusterLines collects the cross-cluster check's lines.
func crossClusterLines(results []CheckResult) []CheckResult {
	var out []CheckResult
	for _, r := range results {
		if r.Check == crossClusterCheck {
			out = append(out, r)
		}
	}
	return out
}

// probeNamespaces returns the names of every namespace on cs carrying
// the probe prefix — the check's own residue signature, and the names a
// foreign lookalike would share.
func probeNamespaces(t *testing.T, cs *fake.Clientset) []string {
	t.Helper()
	list, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ns := range list.Items {
		if strings.HasPrefix(ns.Name, checkNamespacePrefix) {
			out = append(out, ns.Name)
		}
	}
	return out
}

// assertNoProbeResidue fails when any namespace carrying the probe
// prefix remains on any of the clusters.
func assertNoProbeResidue(t *testing.T, byCluster map[string]*fake.Clientset) {
	t.Helper()
	for name, cs := range byCluster {
		if left := probeNamespaces(t, cs); len(left) != 0 {
			t.Errorf("probe namespaces left behind on %s: %v", name, left)
		}
	}
}

// TestVerifyCrossClusterNodePortPassesBothPairs: with two verified
// clusters, each source reaches the other's NodePort through the
// TCP-probe capability with the target's real typed endpoint, the
// probe's objects have the shape kube-proxy needs, and the check leaves
// no namespace behind on either cluster.
func TestVerifyCrossClusterNodePortPassesBothPairs(t *testing.T) {
	// Record the probe namespace the check creates on p-2, so the
	// Service and EndpointSlice can be inspected after cleanup (the
	// fake does not cascade namespace deletion, so the objects remain
	// readable).
	var mu sync.Mutex
	var p2Namespace string
	client, prov, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
		if cluster != "p-2" {
			return
		}
		cs.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
			mu.Lock()
			p2Namespace = action.(k8stesting.CreateAction).GetNamespace()
			mu.Unlock()
			return false, nil, nil
		})
	})
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, r := range results {
		if r.Status != StatusPass && r.Check != crossClusterCheck {
			t.Errorf("check %s/%s = %s (%s), want PASS", r.Cluster, r.Check, r.Status, r.Detail)
		}
	}
	lines := crossClusterLines(results)
	if len(lines) != 2 {
		t.Fatalf("cross-cluster lines = %+v, want one per cluster", lines)
	}
	for _, line := range lines {
		if line.Status != StatusPass {
			t.Errorf("cross-cluster line for %s = %s (%s), want PASS", line.Cluster, line.Status, line.Detail)
		}
		peer := "p-2"
		if line.Cluster == "p-2" {
			peer = "p-1"
		}
		if !strings.Contains(line.Detail, peer) {
			t.Errorf("detail %q must name the peer %s", line.Detail, peer)
		}
	}

	// The connects ran through the capability, once per ordered pair,
	// each carrying its target's typed endpoint.
	wantProbes := []probeCall{
		{cluster: "p-1", addr: netip.MustParseAddr("10.42.0.2"), port: 31002},
		{cluster: "p-2", addr: netip.MustParseAddr("10.42.0.1"), port: 31001},
	}
	if probes := prov.probeList(); !reflect.DeepEqual(probes, wantProbes) {
		t.Errorf("probes = %+v, want one per ordered pair with its target's endpoint %+v", probes, wantProbes)
	}

	// Every target's endpoint was corroborated against the
	// provisioner's own record of where its apiserver listens before
	// anything was created: the PASS rode through the corroboration,
	// not around it.
	if resolved := prov.resolutionList(); !reflect.DeepEqual(resolved, []string{"p-1", "p-2"}) {
		t.Errorf("endpoint corroborations = %v, want one per target cluster [p-1 p-2]", resolved)
	}

	// The Service the check created is selectorless NodePort and its
	// EndpointSlice points at the cluster's apiserver endpoint, under
	// the label kube-proxy resolves slices by.
	mu.Lock()
	nsName := p2Namespace
	mu.Unlock()
	if nsName == "" || !strings.HasPrefix(nsName, checkNamespacePrefix+"-") {
		t.Fatalf("probe namespace = %q, want a generated name under %s-", nsName, checkNamespacePrefix)
	}
	svc, err := byCluster["p-2"].CoreV1().Services(nsName).Get(ctx, nodePortService, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the check's service: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort || svc.Spec.Selector != nil {
		t.Errorf("service = %+v, want a selectorless NodePort service", svc.Spec)
	}
	slice, err := byCluster["p-2"].DiscoveryV1().EndpointSlices(nsName).Get(ctx, nodePortSlice, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the check's EndpointSlice: %v", err)
	}
	if len(slice.Endpoints) != 1 || len(slice.Endpoints[0].Addresses) != 1 ||
		slice.Endpoints[0].Addresses[0] != "10.42.0.2" {
		t.Errorf("slice endpoints = %+v, want the apiserver address 10.42.0.2", slice.Endpoints)
	}
	if slice.Labels[discoveryv1.LabelServiceName] != nodePortService {
		t.Errorf("slice labels = %v, want the service-name label", slice.Labels)
	}
	if len(slice.Ports) != 1 || slice.Ports[0].Port == nil || *slice.Ports[0].Port != 6443 {
		t.Errorf("slice ports = %+v, want the provisioner's API port 6443 as the backend", slice.Ports)
	}

	assertNoProbeResidue(t, byCluster)
}

// TestVerifyCrossClusterNodePortSkipsWithoutTCPProber: a provisioner
// without the TCP-probe capability leaves the check skipped with that
// reason — a runtime that cannot answer the question does not fail it.
func TestVerifyCrossClusterNodePortSkipsWithoutTCPProber(t *testing.T) {
	cfg := testConfig(t, 2, "minimal")
	prov := &lifecycleOnlyProvisioner{inner: newFakeProvisioner()}
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	lines := crossClusterLines(results)
	if len(lines) != 2 {
		t.Fatalf("cross-cluster lines = %+v, want one per cluster", lines)
	}
	for _, line := range lines {
		if line.Status != StatusSkipped || !strings.Contains(line.Detail, "TCP-probe capability") {
			t.Errorf("line for %s = %s (%s), want SKIP naming the missing capability", line.Cluster, line.Status, line.Detail)
		}
	}
}

// TestVerifyCrossClusterNodePortSkipsWithoutAPIEndpointResolver: a
// provisioner that can connect from its nodes but cannot report where
// their apiservers listen leaves the check skipped with that reason —
// an endpoint read from a cluster's API is writable inside that
// cluster, so without the corroboration a PASS could have tested an
// unrelated listener, and the check never runs uncorroborated.
func TestVerifyCrossClusterNodePortSkipsWithoutAPIEndpointResolver(t *testing.T) {
	cfg := testConfig(t, 2, "minimal")
	prov := &probeOnlyProvisioner{inner: newTCPProbeProvisioner()}
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	lines := crossClusterLines(results)
	if len(lines) != 2 {
		t.Fatalf("cross-cluster lines = %+v, want one per cluster", lines)
	}
	for _, line := range lines {
		if line.Status != StatusSkipped || !strings.Contains(line.Detail, "API-endpoint capability") {
			t.Errorf("line for %s = %s (%s), want SKIP naming the missing API-endpoint capability", line.Cluster, line.Status, line.Detail)
		}
	}
	// The capability it does have was never exercised either: no
	// connect ran, because no target was ever prepared.
	if probes := prov.inner.probeList(); len(probes) != 0 {
		t.Errorf("probes = %+v, want none under a skipped check", probes)
	}
}

// probeOnlyProvisioner delegates the lifecycle and the TCP-probe
// capability and deliberately hides the API-endpoint one — the shape
// of a runtime that can connect from its nodes but cannot corroborate
// where their apiservers listen. Each method delegates by name so the
// wrapper implements exactly the interfaces it means to, never more.
type probeOnlyProvisioner struct {
	inner *tcpProbeProvisioner
}

func (p *probeOnlyProvisioner) Create(ctx context.Context, opts provider.CreateOptions) (bool, error) {
	return p.inner.Create(ctx, opts)
}

func (p *probeOnlyProvisioner) Delete(ctx context.Context, name string, kubeconfigPath string) error {
	return p.inner.Delete(ctx, name, kubeconfigPath)
}

func (p *probeOnlyProvisioner) List(ctx context.Context) ([]string, error) {
	return p.inner.List(ctx)
}

func (p *probeOnlyProvisioner) ExportKubeconfig(ctx context.Context, name string, path string) error {
	return p.inner.ExportKubeconfig(ctx, name, path)
}

func (p *probeOnlyProvisioner) ProbeTCPFromNode(ctx context.Context, cluster string, addr netip.Addr, port int) error {
	return p.inner.ProbeTCPFromNode(ctx, cluster, addr, port)
}

// TestVerifyCrossClusterNodePortFailsTheFailingPair: a connect that
// cannot reach the peer FAILs exactly the source's line, naming the
// pair; the healthy direction still passes.
func TestVerifyCrossClusterNodePortFailsTheFailingPair(t *testing.T) {
	client, prov, byCluster := nodePortTestClient(t, nil)
	prov.mu.Lock()
	prov.failFrom["p-1"] = true
	prov.mu.Unlock()
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, line := range crossClusterLines(results) {
		switch line.Cluster {
		case "p-1":
			if line.Status != StatusFail || !strings.Contains(line.Detail, "reaching p-2") {
				t.Errorf("p-1 line = %s (%s), want FAIL naming the pair", line.Status, line.Detail)
			}
		case "p-2":
			if line.Status != StatusPass {
				t.Errorf("p-2 line = %s (%s), want PASS (the healthy direction)", line.Status, line.Detail)
			}
		}
	}
	assertNoProbeResidue(t, byCluster)
}

// TestVerifyCrossClusterNodePortTargetPrepFailure: a target whose
// apiserver endpoint cannot be read fails every source's line with the
// preparation error — a pass over a peer that was never set up would
// hide the missing half — no connect runs against the prepared half,
// and the prepared target's namespace is still cleaned up.
func TestVerifyCrossClusterNodePortTargetPrepFailure(t *testing.T) {
	client, prov, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
		if cluster == "p-2" {
			// No kubernetes EndpointSlice: the endpoint read fails.
			_ = cs.Tracker().Delete(discoveryv1.SchemeGroupVersion.WithResource("endpointslices"), "default", "kubernetes")
		}
	})
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, line := range crossClusterLines(results) {
		if line.Status != StatusFail || !strings.Contains(line.Detail, "p-2") {
			t.Errorf("line for %s = %s (%s), want FAIL naming the unprepared p-2", line.Cluster, line.Status, line.Detail)
		}
	}
	// The check does not spend its probes on a run already failed.
	if probes := prov.probeList(); len(probes) != 0 {
		t.Errorf("probes = %+v, want none when a target failed preparation", probes)
	}
	// p-1 was prepared; its namespace must still be cleaned.
	assertNoProbeResidue(t, byCluster)
}

// TestVerifyCrossClusterNodePortRefusesHostileEndpoint: an endpoint
// address that is not an IP fails the preparation before anything
// reaches a node — the typed endpoint is the injection boundary, and an
// unparseable address cannot even be represented in one.
func TestVerifyCrossClusterNodePortRefusesHostileEndpoint(t *testing.T) {
	client, prov, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
		if cluster == "p-2" {
			_ = cs.Tracker().Delete(discoveryv1.SchemeGroupVersion.WithResource("endpointslices"), "default", "kubernetes")
			_ = cs.Tracker().Add(&discoveryv1.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "kubernetes",
					Namespace: "default",
					Labels:    map[string]string{discoveryv1.LabelServiceName: "kubernetes"},
				},
				AddressType: discoveryv1.AddressTypeIPv4,
				Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"1.2.3.4; touch /tmp/pwned"}}},
				Ports:       []discoveryv1.EndpointPort{{Port: ptrTo(int32(6443))}},
			})
		}
	})
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, line := range crossClusterLines(results) {
		if line.Status != StatusFail || !strings.Contains(line.Detail, "p-2") {
			t.Errorf("line for %s = %s (%s), want FAIL naming the unprepared p-2", line.Cluster, line.Status, line.Detail)
		}
	}
	// The hostile address never reached a node: no probe ran at all.
	if probes := prov.probeList(); len(probes) != 0 {
		t.Errorf("probes = %+v, want none when the endpoint failed validation", probes)
	}
	assertNoProbeResidue(t, byCluster)
}

// TestVerifyCrossClusterNodePortRefusesUncorroboratedEndpoint: an
// endpoint that is well-formed — parseable, routable, exactly what a
// stale or tampered EndpointSlice reports — but is not one of the
// control-plane API endpoints the provisioner reports for the cluster
// (neither its node nor its expected API backend) is refused before
// any probe object is created or any dial happens. The objects
// reporting the endpoint are writable inside the cluster, so the
// endpoint alone does not prove a connect would test this cluster's
// apiserver, and a PASS over it could have tested an unrelated
// listener.
func TestVerifyCrossClusterNodePortRefusesUncorroboratedEndpoint(t *testing.T) {
	t.Run("an endpoint of another listener is refused before any probe object", func(t *testing.T) {
		client, prov, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
			if cluster == "p-2" {
				// A well-formed address that is not p-2's node
				// (10.42.0.2, as the provisioner reports it): the
				// shape a stale or tampered endpoint has — the
				// parse and the port read pass, only the
				// corroboration catches it.
				_ = cs.Tracker().Delete(discoveryv1.SchemeGroupVersion.WithResource("endpointslices"), "default", "kubernetes")
				_ = cs.Tracker().Add(&discoveryv1.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kubernetes",
						Namespace: "default",
						Labels:    map[string]string{discoveryv1.LabelServiceName: "kubernetes"},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"203.0.113.10"}}},
					Ports:       []discoveryv1.EndpointPort{{Port: ptrTo(int32(6443))}},
				})
			}
		})
		ctx := context.Background()
		if _, err := client.Create(ctx); err != nil {
			t.Fatalf("create: %v", err)
		}

		results, err := client.Verify(ctx, nil)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		lines := crossClusterLines(results)
		if len(lines) != 2 {
			t.Fatalf("cross-cluster lines = %+v, want one per cluster", lines)
		}
		for _, line := range lines {
			if line.Status != StatusFail || !strings.Contains(line.Detail, "p-2") {
				t.Errorf("line for %s = %s (%s), want FAIL naming the refused target p-2", line.Cluster, line.Status, line.Detail)
			}
			// The refusal names the uncorroborated endpoint and the
			// provisioner's own one, so a human can see what pointed
			// where.
			for _, substr := range []string{"203.0.113.10:6443", "not one of the control-plane API endpoints", "10.42.0.2:6443"} {
				if !strings.Contains(line.Detail, substr) {
					t.Errorf("line for %s (%s) does not mention %q", line.Cluster, line.Detail, substr)
				}
			}
		}
		// The refusal preceded every probe object and every dial: no
		// connect ran, and p-2 saw no create of any kind — the
		// mismatch was rejected before the namespace was generated.
		if probes := prov.probeList(); len(probes) != 0 {
			t.Errorf("probes = %+v, want none when the endpoint was refused", probes)
		}
		for _, a := range byCluster["p-2"].Actions() {
			if a.GetVerb() == "create" {
				t.Errorf("p-2 saw a %s of %s; the mismatch must be rejected before any probe object exists", a.GetVerb(), a.GetResource().Resource)
			}
		}
		// p-1 was prepared and cleaned; p-2 was never touched.
		assertNoProbeResidue(t, byCluster)
	})

	t.Run("an endpoint resolution failure fails the preparation", func(t *testing.T) {
		client, prov, byCluster := nodePortTestClient(t, nil)
		prov.mu.Lock()
		prov.endpointErr["p-2"] = errors.New("docker daemon is not reachable")
		prov.mu.Unlock()
		ctx := context.Background()
		if _, err := client.Create(ctx); err != nil {
			t.Fatalf("create: %v", err)
		}

		results, err := client.Verify(ctx, nil)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		lines := crossClusterLines(results)
		if len(lines) != 2 {
			t.Fatalf("cross-cluster lines = %+v, want one per cluster", lines)
		}
		for _, line := range lines {
			if line.Status != StatusFail || !strings.Contains(line.Detail, "p-2") ||
				!strings.Contains(line.Detail, "docker daemon is not reachable") {
				t.Errorf("line for %s = %s (%s), want FAIL naming the failed resolution on p-2", line.Cluster, line.Status, line.Detail)
			}
		}
		if probes := prov.probeList(); len(probes) != 0 {
			t.Errorf("probes = %+v, want none when the resolution failed", probes)
		}
		assertNoProbeResidue(t, byCluster)
	})
}

// TestVerifyCrossClusterNodePortRefusesUnmatchedBackendPort: an
// endpoint whose address is the node's own — exactly what the
// provisioner reports — but whose port is a different valid one, the
// shape a stale or tampered EndpointSlice has when it points at
// another listener on the right node, is refused before the first
// write on that target. The check's NodePort backend is bound to the
// matched, provisioner-owned endpoint; a cluster-reported endpoint
// that disagrees on either field means the endpoint is not provably
// the apiserver's, and the old behavior — publishing the reported
// port as the backend — would have reported PASS against whatever
// listener answered, because a TCP connect cannot tell the listeners
// apart.
func TestVerifyCrossClusterNodePortRefusesUnmatchedBackendPort(t *testing.T) {
	t.Run("an endpoint port of another listener is refused before any probe object", func(t *testing.T) {
		client, prov, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
			if cluster == "p-2" {
				// A well-formed endpoint on exactly the corroborated
				// node address (10.42.0.2, as the provisioner reports
				// it) whose port names a different valid listener: the
				// address half matches, and only the endpoint match
				// catches it.
				_ = cs.Tracker().Delete(discoveryv1.SchemeGroupVersion.WithResource("endpointslices"), "default", "kubernetes")
				_ = cs.Tracker().Add(&discoveryv1.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kubernetes",
						Namespace: "default",
						Labels:    map[string]string{discoveryv1.LabelServiceName: "kubernetes"},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.42.0.2"}}},
					Ports:       []discoveryv1.EndpointPort{{Port: ptrTo(int32(8443))}},
				})
			}
		})
		ctx := context.Background()
		if _, err := client.Create(ctx); err != nil {
			t.Fatalf("create: %v", err)
		}

		results, err := client.Verify(ctx, nil)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		lines := crossClusterLines(results)
		if len(lines) != 2 {
			t.Fatalf("cross-cluster lines = %+v, want one per cluster", lines)
		}
		for _, line := range lines {
			if line.Status != StatusFail || !strings.Contains(line.Detail, "p-2") {
				t.Errorf("line for %s = %s (%s), want FAIL naming the refused target p-2", line.Cluster, line.Status, line.Detail)
			}
			// The refusal names both endpoints, so a human can see
			// what pointed where.
			for _, substr := range []string{"10.42.0.2:8443", "not one of the control-plane API endpoints", "10.42.0.2:6443"} {
				if !strings.Contains(line.Detail, substr) {
					t.Errorf("line for %s (%s) does not mention %q", line.Cluster, line.Detail, substr)
				}
			}
		}
		// The refusal preceded every probe object and every dial: no
		// connect ran, and p-2 saw no create of any kind — the
		// mismatch was rejected before the namespace was generated.
		if probes := prov.probeList(); len(probes) != 0 {
			t.Errorf("probes = %+v, want none when the backend port was not the provisioner's", probes)
		}
		for _, a := range byCluster["p-2"].Actions() {
			if a.GetVerb() == "create" {
				t.Errorf("p-2 saw a %s of %s; the mismatch must be rejected before any probe object exists", a.GetVerb(), a.GetResource().Resource)
			}
		}
		// p-1 was prepared and cleaned; p-2 was never touched.
		assertNoProbeResidue(t, byCluster)
	})

	// The resolver-failure subtest lives with the endpoint-corroboration
	// refusal above (TestVerifyCrossClusterNodePortRefusesUncorroboratedEndpoint,
	// "an endpoint resolution failure fails the preparation"): the two
	// tests share one resolver capability, and the
	// duplicated coverage there is the kept one.
}

// TestVerifyCrossClusterNodePortRefusesOutOfRangePort: an endpoint
// port outside 1-65535 — the EndpointSlice's port is an int32, and
// the check narrows it to the endpoint type (netip.AddrPort's uint16)
// only after this guard — is refused before the first write on the
// target. The crafted case is the provisioner's port plus 2^16: a
// narrowing truncation would turn it into exactly the provisioner's
// port and corroborate a tampered endpoint, so the guard must refuse
// the value rather than narrow it. (The resolver's endpoints cannot
// carry an out-of-range port — an AddrPort's port is a uint16 — so
// the cluster-reported side is the only one that needs the guard.)
func TestVerifyCrossClusterNodePortRefusesOutOfRangePort(t *testing.T) {
	for _, tc := range []struct {
		name string
		port int32
	}{
		{"zero", 0},
		{"negative", -1},
		{"above the range", 65536},
		{"the provisioner's port plus 2^16, which a uint16 narrowing would turn into a match", 6443 + 65536},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, prov, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
				if cluster == "p-2" {
					// The corroborated node address with a port no
					// listener can hold: well-formed enough to reach
					// the corroboration, impossible as an endpoint.
					_ = cs.Tracker().Delete(discoveryv1.SchemeGroupVersion.WithResource("endpointslices"), "default", "kubernetes")
					_ = cs.Tracker().Add(&discoveryv1.EndpointSlice{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "kubernetes",
							Namespace: "default",
							Labels:    map[string]string{discoveryv1.LabelServiceName: "kubernetes"},
						},
						AddressType: discoveryv1.AddressTypeIPv4,
						Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.42.0.2"}}},
						Ports:       []discoveryv1.EndpointPort{{Port: ptrTo(tc.port)}},
					})
				}
			})
			ctx := context.Background()
			if _, err := client.Create(ctx); err != nil {
				t.Fatalf("create: %v", err)
			}

			results, err := client.Verify(ctx, nil)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			lines := crossClusterLines(results)
			if len(lines) != 2 {
				t.Fatalf("cross-cluster lines = %+v, want one per cluster", lines)
			}
			for _, line := range lines {
				if line.Status != StatusFail || !strings.Contains(line.Detail, "p-2") ||
					!strings.Contains(line.Detail, "outside the valid range 1-65535") {
					t.Errorf("line for %s = %s (%s), want FAIL naming the out-of-range port on p-2", line.Cluster, line.Status, line.Detail)
				}
			}
			// The refusal preceded every probe object and every dial.
			if probes := prov.probeList(); len(probes) != 0 {
				t.Errorf("probes = %+v, want none when the port was out of range", probes)
			}
			for _, a := range byCluster["p-2"].Actions() {
				if a.GetVerb() == "create" {
					t.Errorf("p-2 saw a %s of %s; the out-of-range port must be refused before any write", a.GetVerb(), a.GetResource().Resource)
				}
			}
			assertNoProbeResidue(t, byCluster)
		})
	}
}

// TestVerifyCrossClusterNodePortReportsLeftBehindNamespace: a namespace
// that will not go away FAILs the owning cluster's own line, folded
// into that line's verdict — never a second line after a PASS — while
// the healthy cluster's line keeps its own verdict.
func TestVerifyCrossClusterNodePortReportsLeftBehindNamespace(t *testing.T) {
	// Drive the deletion wait short: the stuck namespace must fail in
	// milliseconds, not after the production minute.
	oldTimeout := nsDeleteTimeout
	nsDeleteTimeout = 100 * time.Millisecond
	t.Cleanup(func() { nsDeleteTimeout = oldTimeout })

	client, _, _ := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
		if cluster == "p-1" {
			// A namespace stuck in Terminating: the delete is accepted,
			// but the GET keeps answering the instance the check
			// created (the fake tracker assigns no UIDs, so the
			// created instance's UID is the empty one answered here —
			// the read-side identity the confirmation matches on).
			cs.PrependReactor("get", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
				name := action.(k8stesting.GetAction).GetName()
				if !strings.HasPrefix(name, checkNamespacePrefix) {
					return false, nil, nil
				}
				return true, &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: name},
					Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating},
				}, nil
			})
		}
	})
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	lines := crossClusterLines(results)
	if len(lines) != 2 {
		t.Fatalf("cross-cluster lines = %+v, want exactly one per cluster (the cleanup failure folds into the owning line)", lines)
	}
	for _, line := range lines {
		switch line.Cluster {
		case "p-1":
			if line.Status != StatusFail || !strings.Contains(line.Detail, "left its probe state behind") {
				t.Errorf("p-1 line = %s (%s), want the folded cleanup FAIL", line.Status, line.Detail)
			}
		case "p-2":
			if line.Status != StatusPass {
				t.Errorf("p-2 line = %s (%s), want PASS (its own cleanup succeeded)", line.Status, line.Detail)
			}
		}
	}
}

// TestVerifyCrossClusterNodePortSparesForeignNamespaces: namespaces
// that merely look like the check's residue — the previous design's
// fixed name, and a plausible killed-run leftover — are never adopted
// or removed. The unique per-probe name makes collision impossible, so
// the check passes over them and leaves them exactly as it found them;
// deleting by name or prefix alone would remove state the run cannot
// prove it owns.
func TestVerifyCrossClusterNodePortSparesForeignNamespaces(t *testing.T) {
	client, _, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
		if cluster == "p-1" {
			// The residue a killed run of the old, fixed-name design
			// would leave, plus a lookalike of the generated shape.
			_ = cs.Tracker().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: checkNamespacePrefix}})
			_ = cs.Tracker().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: checkNamespacePrefix + "-0123456789ab"}})
		}
	})
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, line := range crossClusterLines(results) {
		if line.Status != StatusPass {
			t.Errorf("line for %s = %s (%s), want PASS over the foreign lookalikes", line.Cluster, line.Status, line.Detail)
		}
	}
	// The foreign namespaces survive untouched; only the check's own
	// (differently random) namespace came and went.
	left := probeNamespaces(t, byCluster["p-1"])
	want := []string{checkNamespacePrefix, checkNamespacePrefix + "-0123456789ab"}
	if !reflect.DeepEqual(left, want) {
		t.Errorf("prefix namespaces on p-1 after verify = %v, want exactly the foreign ones %v", left, want)
	}
	if left := probeNamespaces(t, byCluster["p-2"]); len(left) != 0 {
		t.Errorf("probe namespaces on p-2 after verify = %v, want none", left)
	}
}

// TestCrossClusterUnverifiedClusterSkips: a cluster that failed its
// identity check gets a SKIP line, not a reachability verdict built on
// a kubeconfig that cannot be trusted.
func TestCrossClusterUnverifiedClusterSkips(t *testing.T) {
	client, prov, byCluster := nodePortTestClient(t, nil)
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Between create and verify, p-2's cluster is replaced under the
	// same name: its kube-system namespace answers with a new UID, so
	// verify's identity check fails for p-2 — the same shape a
	// deleted-and-recreated cluster has.
	byCluster["p-2"].PrependReactor("get", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.GetAction).GetName() != "kube-system" {
			return false, nil, nil
		}
		return true, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "kube-system", UID: "uid-replacement-instance",
		}}, nil
	})

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	lines := crossClusterLines(results)
	if len(lines) != 2 {
		t.Fatalf("cross-cluster lines = %+v, want one per cluster", lines)
	}
	for _, line := range lines {
		switch line.Cluster {
		case "p-1":
			if line.Status != StatusSkipped || !strings.Contains(line.Detail, "no other verified cluster") {
				t.Errorf("p-1 line = %s (%s), want SKIP (its only peer is unverified)", line.Status, line.Detail)
			}
		case "p-2":
			if line.Status != StatusSkipped || !strings.Contains(line.Detail, "not verified") {
				t.Errorf("p-2 line = %s (%s), want SKIP naming the failed verification", line.Status, line.Detail)
			}
		}
	}
	if probes := prov.probeList(); len(probes) != 0 {
		t.Errorf("probes = %+v, want none against an unverified cluster", probes)
	}
}

// TestVerifyCrossClusterNodePortCleansPartialTargetOnSetupFailure: a
// preparation that fails after the namespace exists (here: the Service
// create is refused) still returns the target, and the cleanup removes
// the namespace — the check never abandons a namespace it created.
func TestVerifyCrossClusterNodePortCleansPartialTargetOnSetupFailure(t *testing.T) {
	client, prov, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
		if cluster == "p-2" {
			cs.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("apiserver refused the service")
			})
		}
	})
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, line := range crossClusterLines(results) {
		if line.Status != StatusFail || !strings.Contains(line.Detail, "p-2") {
			t.Errorf("line for %s = %s (%s), want FAIL naming the failed preparation on p-2", line.Cluster, line.Status, line.Detail)
		}
	}
	if probes := prov.probeList(); len(probes) != 0 {
		t.Errorf("probes = %+v, want none when a target failed preparation", probes)
	}
	// Both clusters' namespaces — p-2's partial one included — are gone.
	assertNoProbeResidue(t, byCluster)
}

// TestVerifyCrossClusterNodePortCleansUpWhenCancelled: cancelling the
// verify context during the connect phase fails the connects, but the
// cleanup still runs — it carries its own bounded context, never the
// check's — so no residue survives the cancellation.
func TestVerifyCrossClusterNodePortCleansUpWhenCancelled(t *testing.T) {
	client, prov, byCluster := nodePortTestClient(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov.mu.Lock()
	prov.onProbe = func(probeCall) error {
		// The cancellation lands mid-connect, as an interrupted run
		// would deliver it.
		cancel()
		return errors.New("connect interrupted: the verify run was cancelled")
	}
	prov.mu.Unlock()

	if _, err := client.Create(context.Background()); err != nil {
		t.Fatalf("create: %v", err)
	}
	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, line := range crossClusterLines(results) {
		if line.Status != StatusFail || !strings.Contains(line.Detail, "reaching") {
			t.Errorf("line for %s = %s (%s), want FAIL naming the interrupted connect", line.Cluster, line.Status, line.Detail)
		}
		if strings.Contains(line.Detail, "left its probe namespace") || strings.Contains(line.Detail, "cleaning up") {
			t.Errorf("line for %s reports a cleanup failure (%s); cleanup must not inherit the cancelled context", line.Cluster, line.Detail)
		}
	}
	assertNoProbeResidue(t, byCluster)
}

// TestVerifyCrossClusterNodePortRefusesSwappedKubeconfig: a kubeconfig
// replaced between verify's identity check and the check's target
// preparation is a refusal, never a mutation of the replacement — the
// identity is re-read through the very client the mutation would run
// through. The swap here lands exactly between the two reads.
func TestVerifyCrossClusterNodePortRefusesSwappedKubeconfig(t *testing.T) {
	cfg := testConfig(t, 2, "minimal")
	prov := newTCPProbeProvisioner()
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	byCluster := map[string]*fake.Clientset{
		"p-1": nodePortClientset("10.42.0.1", 31001, nil),
		"p-2": nodePortClientset("10.42.0.2", 31002, nil),
	}
	// The honest endpoint tables for both clusters, so p-1's
	// preparation genuinely completes and the kubeconfig swap is the
	// only failure: without these, p-1 would fail its own endpoint
	// corroboration and the pair would never be exercised as written.
	prov.mu.Lock()
	prov.apiEndpoints["p-1"] = []netip.AddrPort{netip.MustParseAddrPort("10.42.0.1:6443")}
	prov.apiEndpoints["p-2"] = []netip.AddrPort{netip.MustParseAddrPort("10.42.0.2:6443")}
	prov.mu.Unlock()
	replacement := clientsetFor("uid-replacement-instance", corev1.ConditionTrue)
	calls := map[string]int{}
	client.clientset = func(path string) (kubernetes.Interface, error) {
		name := ""
		for _, n := range []string{"p-1", "p-2"} {
			if strings.Contains(path, "/"+n+"/") {
				name = n
			}
		}
		if name == "" {
			return nil, fmt.Errorf("no fake apiserver for kubeconfig %s", path)
		}
		calls[name]++
		// p-2's clientset constructions after the identity check (its
		// second) hand back the replacement: the kubeconfig file was
		// swapped to a same-name cluster in between.
		if name == "p-2" && calls["p-2"] >= 3 {
			return replacement, nil
		}
		return byCluster[name], nil
	}

	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	calls = map[string]int{} // count verify's constructions only

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, line := range crossClusterLines(results) {
		if line.Status != StatusFail || !strings.Contains(line.Detail, "p-2") ||
			!strings.Contains(line.Detail, "different cluster instance") {
			t.Errorf("line for %s = %s (%s), want FAIL naming the swapped kubeconfig on p-2", line.Cluster, line.Status, line.Detail)
		}
	}
	// The replacement cluster was read (the identity re-check) but
	// never mutated: no create or delete reached it.
	for _, a := range replacement.Actions() {
		if a.GetVerb() == "create" || a.GetVerb() == "delete" {
			t.Errorf("the replacement cluster saw %s %s; the refusal must gate every mutation", a.GetVerb(), a.GetResource().Resource)
		}
	}
	if probes := prov.probeList(); len(probes) != 0 {
		t.Errorf("probes = %+v, want none when a target was refused", probes)
	}
	// p-1 was prepared before the refusal; its namespace is cleaned.
	assertNoProbeResidue(t, byCluster)
}

// TestVerifyCrossClusterNodePortSparesReplacedProbeNamespace: a probe
// namespace replaced under the check (deleted and re-created under the
// same name while the check runs) is not the check's to remove: the
// UID-preconditioned delete is refused by the API and the replacement
// survives. The UID re-check before each child write refuses to write
// into the replacement when it can see it, and the child that did land
// in one — the race between a re-check and the replacement, which no
// check can close — is removed as this check's own: by its UID when
// the create answered, and by the attempt marker when its response
// was lost. The delete reactors enforce the UID preconditions the
// real apiserver enforces (the fake tracker does not).
func TestVerifyCrossClusterNodePortSparesReplacedProbeNamespace(t *testing.T) {
	const (
		replacementUID   = "uid-replaced-probe-namespace"
		probeServiceUID  = "uid-probe-service"
		unrelatedService = "unrelated-service"
	)

	// replaceWith lands the out-of-band replacement: the namespace is
	// removed and re-created under the same name with a new UID,
	// carrying an unrelated object of its own.
	replaceWith := func(cs *fake.Clientset, ns string) {
		_ = cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("namespaces"), "", ns)
		_ = cs.Tracker().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: replacementUID}})
		_ = cs.Tracker().Add(&corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: unrelatedService, Namespace: ns, UID: "uid-unrelated-service",
		}})
	}

	// assertReplacementIntact pins what every subtest must leave
	// behind: exactly the replacement under the probe name, and the
	// unrelated object in it. It returns the replacement's name.
	assertReplacementIntact := func(t *testing.T, cs *fake.Clientset) string {
		t.Helper()
		left := probeNamespaces(t, cs)
		if len(left) != 1 {
			t.Fatalf("prefix namespaces = %v, want exactly the replacement", left)
		}
		nsName := left[0]
		ns, err := cs.CoreV1().Namespaces().Get(context.Background(), nsName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if string(ns.UID) != replacementUID {
			t.Errorf("namespace %s UID = %q, want the replacement's %q — the check must not delete state it cannot prove it owns", nsName, ns.UID, replacementUID)
		}
		if _, err := cs.CoreV1().Services(nsName).Get(context.Background(), unrelatedService, metav1.GetOptions{}); err != nil {
			t.Errorf("the unrelated service must survive the cleanup: %v", err)
		}
		return nsName
	}

	// assertNoProbeChildren pins that nothing of the check's remains
	// in the replacement: the child Service name answers NotFound and
	// no EndpointSlice of the check's is left.
	assertNoProbeChildren := func(t *testing.T, cs *fake.Clientset, nsName string) {
		t.Helper()
		ctx := context.Background()
		if _, err := cs.CoreV1().Services(nsName).Get(ctx, nodePortService, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("reading the check's service in the replacement namespace: %v, want NotFound (removed as this check's own)", err)
		}
		sliceList, err := cs.DiscoveryV1().EndpointSlices(nsName).List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(sliceList.Items) != 0 {
			t.Errorf("EndpointSlices in the replacement namespace = %d, want none of the check's", len(sliceList.Items))
		}
	}

	t.Run("a child that answered is removed by its UID", func(t *testing.T) {
		client, _, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
			if cluster != "p-1" {
				return
			}
			enforceDeleteUID(cs, "namespaces")
			enforceDeleteUID(cs, "services")
			// The replacement lands while the check creates its
			// Service (the namespace already exists, the re-check
			// before the write passed), so the create proceeds into
			// it — the race — and its UID is what must distinguish
			// the child at cleanup.
			cs.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
				create := action.(k8stesting.CreateAction)
				svc := create.GetObject().(*corev1.Service)
				if svc.Name == nodePortService {
					svc.UID = probeServiceUID
				}
				replaceWith(cs, create.GetNamespace())
				return false, nil, nil
			})
		})
		ctx := context.Background()
		if _, err := client.Create(ctx); err != nil {
			t.Fatalf("create: %v", err)
		}

		results, err := client.Verify(ctx, nil)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		for _, line := range crossClusterLines(results) {
			// The preparation refused to finish against the
			// replacement (the re-check before the EndpointSlice) and
			// its residue was cleaned by UID: the line FAILs on the
			// preparation, never on state left behind.
			if line.Status != StatusFail || !strings.Contains(line.Detail, "replaced while the check ran") {
				t.Errorf("line for %s = %s (%s), want FAIL naming the replaced namespace", line.Cluster, line.Status, line.Detail)
			}
			if strings.Contains(line.Detail, "left its probe state behind") {
				t.Errorf("line for %s reports leftover residue (%s); the probe's children must be cleaned as this check's own", line.Cluster, line.Detail)
			}
		}
		nsName := assertReplacementIntact(t, byCluster["p-1"])
		assertNoProbeChildren(t, byCluster["p-1"], nsName)
	})

	t.Run("a child whose create lost its response is resolved by its marker", func(t *testing.T) {
		client, _, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
			if cluster != "p-1" {
				return
			}
			enforceDeleteUID(cs, "namespaces")
			enforceDeleteUID(cs, "services")
			// The same race with the response lost on top: the Service
			// is persisted — carrying this attempt's marker, the labels
			// its create sent — but the create returns an error, so
			// the child's UID is never recorded. The marker is what
			// must settle the child at cleanup, and the observed UID
			// is what the removal is preconditioned on.
			cs.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
				create := action.(k8stesting.CreateAction)
				svc := create.GetObject().(*corev1.Service)
				replaceWith(cs, create.GetNamespace())
				if svc.Name != nodePortService {
					return false, nil, nil
				}
				svc.UID = probeServiceUID
				// The server persists the create...
				if err := cs.Tracker().Create(create.GetResource(), svc, svc.GetNamespace()); err != nil {
					return true, nil, err
				}
				// ...and the response is lost.
				return true, nil, errors.New("net/http: request canceled (Client.Timeout exceeded while reading body)")
			})
		})
		ctx := context.Background()
		if _, err := client.Create(ctx); err != nil {
			t.Fatalf("create: %v", err)
		}

		results, err := client.Verify(ctx, nil)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		for _, line := range crossClusterLines(results) {
			if line.Status != StatusFail || !strings.Contains(line.Detail, "creating the NodePort service") {
				t.Errorf("line for %s = %s (%s), want FAIL naming the lost service create", line.Cluster, line.Status, line.Detail)
			}
			if strings.Contains(line.Detail, "left its probe state behind") {
				t.Errorf("line for %s reports leftover residue (%s); the marker-resolved child must be cleaned as this check's own", line.Cluster, line.Detail)
			}
		}
		nsName := assertReplacementIntact(t, byCluster["p-1"])
		assertNoProbeChildren(t, byCluster["p-1"], nsName)
	})

	t.Run("a foreign holder of the child's name is left alone", func(t *testing.T) {
		client, _, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
			if cluster != "p-1" {
				return
			}
			enforceDeleteUID(cs, "namespaces")
			enforceDeleteUID(cs, "services")
			// The replacement carries its own service under the very
			// name the check's child would take, so the child create
			// is refused with an AlreadyExists and its outcome is
			// resolved by the marker: the holder carries none, so it
			// is not this check's and must survive.
			cs.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
				create := action.(k8stesting.CreateAction)
				ns := create.GetNamespace()
				replaceWith(cs, ns)
				if create.GetObject().(*corev1.Service).Name != nodePortService {
					return false, nil, nil
				}
				_ = cs.Tracker().Add(&corev1.Service{ObjectMeta: metav1.ObjectMeta{
					Name:      nodePortService,
					Namespace: ns,
					UID:       "uid-foreign-service",
				}})
				return true, nil, apierrors.NewAlreadyExists(corev1.Resource("services"), nodePortService)
			})
		})
		ctx := context.Background()
		if _, err := client.Create(ctx); err != nil {
			t.Fatalf("create: %v", err)
		}

		results, err := client.Verify(ctx, nil)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		for _, line := range crossClusterLines(results) {
			if line.Status != StatusFail || !strings.Contains(line.Detail, "creating the NodePort service") {
				t.Errorf("line for %s = %s (%s), want FAIL naming the refused service create", line.Cluster, line.Status, line.Detail)
			}
			if strings.Contains(line.Detail, "left its probe state behind") {
				t.Errorf("line for %s reports leftover residue (%s); a foreign holder of the child's name is not this check's to report", line.Cluster, line.Detail)
			}
		}
		nsName := assertReplacementIntact(t, byCluster["p-1"])
		// Only a marker-matched child is this check's to remove: the
		// foreign holder survives the resolution untouched.
		svc, err := byCluster["p-1"].CoreV1().Services(nsName).Get(ctx, nodePortService, metav1.GetOptions{})
		if err != nil || string(svc.UID) != "uid-foreign-service" {
			t.Errorf("the foreign service under the child's name = (UID %q, %v), want it untouched", svc.UID, err)
		}
		if left := probeNamespaces(t, byCluster["p-2"]); len(left) != 0 {
			t.Errorf("probe namespaces on p-2 = %v, want none", left)
		}
	})
}

// TestVerifyCrossClusterNodePortReportsRefusedNamespaceDelete: a 409
// on the namespace delete is not proof the name was replaced — the
// API answers an admission refusal the same way, with the original
// still served. The cleanup must confirm by reading, and a namespace
// still serving the check's UID FAILs the owning line instead of
// passing vacuously. (The old cleanup read every 409 as a replacement:
// it would have deleted the children out of the check's own live
// namespace and reported PASS over the leftover.)
func TestVerifyCrossClusterNodePortReportsRefusedNamespaceDelete(t *testing.T) {
	// Drive the confirmation wait short: the refused namespace must
	// fail in milliseconds, not after the production minute.
	oldTimeout := nsDeleteTimeout
	nsDeleteTimeout = 100 * time.Millisecond
	t.Cleanup(func() { nsDeleteTimeout = oldTimeout })

	client, _, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
		if cluster != "p-1" {
			return
		}
		// The API answers an admission refusal with a 409 while the
		// original namespace is still served — the same status a
		// replacement's UID mismatch produces.
		cs.PrependReactor("delete", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
			name := action.(k8stesting.DeleteAction).GetName()
			if !strings.HasPrefix(name, checkNamespacePrefix) {
				return false, nil, nil
			}
			return true, nil, apierrors.NewConflict(
				corev1.Resource("namespaces"), name,
				errors.New(`admission webhook "deny.example.com" denied the request`))
		})
	})
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, line := range crossClusterLines(results) {
		switch line.Cluster {
		case "p-1":
			if line.Status != StatusFail || !strings.Contains(line.Detail, "left its probe state behind") {
				t.Errorf("p-1 line = %s (%s), want the folded cleanup FAIL: the refused delete is not a replacement", line.Status, line.Detail)
			}
		case "p-2":
			if line.Status != StatusPass {
				t.Errorf("p-2 line = %s (%s), want PASS (its own cleanup succeeded)", line.Status, line.Detail)
			}
		}
	}
	// The namespace is still served and still whole: the 409 must not
	// send the cleanup down the replacement path, where it would strip
	// the check's children out of the check's own namespace.
	left := probeNamespaces(t, byCluster["p-1"])
	if len(left) != 1 {
		t.Fatalf("prefix namespaces on p-1 = %v, want the refused-delete residue reported, not silently passed over", left)
	}
	if _, err := byCluster["p-1"].CoreV1().Services(left[0]).Get(ctx, nodePortService, metav1.GetOptions{}); err != nil {
		t.Errorf("the check's service in its own namespace: %v, want it untouched (the 409 was not a replacement)", err)
	}
	if left := probeNamespaces(t, byCluster["p-2"]); len(left) != 0 {
		t.Errorf("probe namespaces on p-2 = %v, want none", left)
	}
}

// TestVerifyCrossClusterNodePortConfirmsChildRemoval: a child delete
// the API accepts is not proof the child is gone — finalizers hold
// objects past the call's return — so the cleanup confirms each child
// by reading it, and a child still serving its UID FAILs the owning
// line. The second child is still attempted (and here removed) when
// the first one's confirmation fails.
func TestVerifyCrossClusterNodePortConfirmsChildRemoval(t *testing.T) {
	const (
		replacementUID   = "uid-replaced-probe-namespace"
		probeServiceUID  = "uid-probe-service"
		probeSliceUID    = "uid-probe-slice"
		unrelatedService = "unrelated-service"
	)
	// Drive the confirmation wait short: the stuck child must fail in
	// milliseconds, not after the production minute.
	oldTimeout := nsDeleteTimeout
	nsDeleteTimeout = 100 * time.Millisecond
	t.Cleanup(func() { nsDeleteTimeout = oldTimeout })

	client, _, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
		if cluster != "p-1" {
			return
		}
		enforceDeleteUID(cs, "namespaces")
		enforceDeleteUID(cs, "services")
		enforceDeleteUID(cs, "endpointslices")
		// Both children carry known UIDs (their creates answer).
		cs.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
			action.(k8stesting.CreateAction).GetObject().(*corev1.Service).UID = probeServiceUID
			return false, nil, nil
		})
		// The replacement lands at the last child write: both children
		// were issued, and the name is then held by an instance the
		// check did not create — so the cleanup must take the
		// children path.
		cs.PrependReactor("create", "endpointslices", func(action k8stesting.Action) (bool, runtime.Object, error) {
			create := action.(k8stesting.CreateAction)
			create.GetObject().(*discoveryv1.EndpointSlice).UID = probeSliceUID
			ns := create.GetNamespace()
			_ = cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("namespaces"), "", ns)
			_ = cs.Tracker().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: replacementUID}})
			_ = cs.Tracker().Add(&corev1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: unrelatedService, Namespace: ns, UID: "uid-unrelated-service",
			}})
			return false, nil, nil
		})
		// The service delete is accepted — and the object stays: a
		// finalizer holding it past the call's return. Prepended
		// before the enforcers, so the UID check still runs first.
		cs.PrependReactor("delete", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, nil
		})
	})
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, line := range crossClusterLines(results) {
		switch line.Cluster {
		case "p-1":
			if line.Status != StatusFail ||
				!strings.Contains(line.Detail, "left its probe state behind") ||
				!strings.Contains(line.Detail, "confirming the service") {
				t.Errorf("p-1 line = %s (%s), want the folded cleanup FAIL naming the unconfirmed service removal", line.Status, line.Detail)
			}
		case "p-2":
			if line.Status != StatusPass {
				t.Errorf("p-2 line = %s (%s), want PASS (its own cleanup succeeded)", line.Status, line.Detail)
			}
		}
	}
	// The replacement survived and is the only prefix namespace on
	// p-1; the unrelated object in it survived with it.
	left := probeNamespaces(t, byCluster["p-1"])
	if len(left) != 1 {
		t.Fatalf("prefix namespaces on p-1 = %v, want exactly the replacement", left)
	}
	nsName := left[0]
	if ns, err := byCluster["p-1"].CoreV1().Namespaces().Get(ctx, nsName, metav1.GetOptions{}); err != nil || string(ns.UID) != replacementUID {
		t.Errorf("replacement namespace = (UID %q, %v), want it untouched with UID %q", ns.UID, err, replacementUID)
	}
	if _, err := byCluster["p-1"].CoreV1().Services(nsName).Get(ctx, unrelatedService, metav1.GetOptions{}); err != nil {
		t.Errorf("the unrelated service must survive the cleanup: %v", err)
	}
	// The service the API would not finish removing is still there —
	// reported, not assumed gone — while the second child, attempted
	// despite the first one's failure, is gone.
	svc, err := byCluster["p-1"].CoreV1().Services(nsName).Get(ctx, nodePortService, metav1.GetOptions{})
	if err != nil || string(svc.UID) != probeServiceUID {
		t.Errorf("the stuck service = (UID %q, %v), want it still present under %q and honestly reported", svc.UID, err, probeServiceUID)
	}
	if _, err := byCluster["p-1"].DiscoveryV1().EndpointSlices(nsName).Get(ctx, nodePortSlice, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("reading the check's EndpointSlice: %v, want NotFound (the second child is attempted despite the first one's failure)", err)
	}
	if left := probeNamespaces(t, byCluster["p-2"]); len(left) != 0 {
		t.Errorf("probe namespaces on p-2 = %v, want none", left)
	}
}

// TestCleanNodePortTargetIssuesEveryChildDeleteUnderTheDeadline: both
// children are cleaned at the same time under the one cleanup
// deadline, because a child's confirmation can consume that whole
// deadline — a finalizer holding the object past its accepted delete —
// and a child cleaned second would find the deadline spent: the
// transport refuses its delete locally, and it never reaches the API.
// The regression is transport-aware for exactly that reason — the fake
// clientset ignores an expired context, which is why the fake-based
// regressions cannot see the starvation — so it drives the real
// client-go transport against a local HTTP apiserver: the Service
// delete is accepted while the Service keeps serving its UID, so its
// confirmation polls to the deadline's end; the EndpointSlice delete
// must still reach the server under its UID precondition and be
// confirmed gone, and the Service failure must still be reported.
func TestCleanNodePortTargetIssuesEveryChildDeleteUnderTheDeadline(t *testing.T) {
	const (
		nsName           = checkNamespacePrefix + "-deadbeefcafe"
		originalNsUID    = types.UID("uid-original-namespace")
		replacementNsUID = types.UID("uid-replaced-namespace")
		probeServiceUID  = types.UID("uid-probe-service")
		probeSliceUID    = types.UID("uid-probe-slice")
	)
	// Long enough that localhost round trips cannot eat it, short
	// enough to keep the regression quick: the Service confirmation
	// polls to this deadline either way.
	oldTimeout := nsDeleteTimeout
	nsDeleteTimeout = time.Second
	t.Cleanup(func() { nsDeleteTimeout = oldTimeout })

	var mu sync.Mutex
	var nsDeletes, serviceDeletes, sliceDeletes []metav1.DeleteOptions
	writeObject := func(w http.ResponseWriter, obj runtime.Object) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(obj); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordDelete := func(into *[]metav1.DeleteOptions) {
			var opts metav1.DeleteOptions
			_ = json.NewDecoder(r.Body).Decode(&opts)
			mu.Lock()
			*into = append(*into, opts)
			mu.Unlock()
		}
		svcPath := "/api/v1/namespaces/" + nsName + "/services/" + nodePortService
		slicePath := "/apis/discovery.k8s.io/v1/namespaces/" + nsName + "/endpointslices/" + nodePortSlice
		switch r.Method + " " + r.URL.Path {
		case "DELETE /api/v1/namespaces/" + nsName:
			recordDelete(&nsDeletes)
		case "GET /api/v1/namespaces/" + nsName:
			// A different instance holds the name: the children path.
			writeObject(w, &corev1.Namespace{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
				ObjectMeta: metav1.ObjectMeta{Name: nsName, UID: replacementNsUID},
			})
		case "DELETE " + svcPath:
			recordDelete(&serviceDeletes)
		case "GET " + svcPath:
			// The delete was accepted, and the terminating UID keeps
			// serving: the confirmation polls to the deadline's end.
			now := metav1.Now()
			writeObject(w, &corev1.Service{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
				ObjectMeta: metav1.ObjectMeta{Name: nodePortService, Namespace: nsName, UID: probeServiceUID, DeletionTimestamp: &now},
			})
		case "DELETE " + slicePath:
			recordDelete(&sliceDeletes)
		case "GET " + slicePath:
			status := apierrors.NewNotFound(discoveryv1.Resource("endpointslices"), nodePortSlice).Status()
			status.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(status)
		default:
			t.Errorf("unexpected API call: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected call", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	// The clientset speaks JSON for this test's apiserver (the typed
	// client's default is protobuf, which the assertions would have
	// to decode); the cleanup passes the same typed calls either way.
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL, ContentType: "application/json"})
	if err != nil {
		t.Fatalf("building the apiserver client: %v", err)
	}
	target := nodePortTarget{
		peer:   verifiedCluster{name: "p-1"},
		client: cs,
		nsName: nsName,
		nsUID:  originalNsUID,
		marker: "deadbeef",
		svc:    probeChild{kind: "service", name: nodePortService, uid: probeServiceUID, issued: true},
		slice:  probeChild{kind: "endpointslice", name: nodePortSlice, uid: probeSliceUID, issued: true},
	}
	err = (&Client{}).cleanNodePortTarget(target)
	if err == nil {
		t.Fatal("cleanNodePortTarget = nil error, want the Service confirmation failure reported")
	}
	for _, substr := range []string{"removing the check's service", "confirming the service"} {
		if !strings.Contains(err.Error(), substr) {
			t.Errorf("error %q does not mention %q", err, substr)
		}
	}
	if strings.Contains(err.Error(), "endpointslice") {
		t.Errorf("error %q reports the EndpointSlice, want it deleted and confirmed under the deadline", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// The exact counts first: each deletion is issued exactly once —
	// a UID-preconditioned delete re-sent by a retry would appear
	// here even when the UID assertions below still hold.
	if len(nsDeletes) != 1 || len(serviceDeletes) != 1 || len(sliceDeletes) != 1 {
		t.Errorf("DELETEs reaching the server: namespace=%d service=%d endpointslice=%d, want exactly one of each",
			len(nsDeletes), len(serviceDeletes), len(sliceDeletes))
	}
	uidOf := func(deletes []metav1.DeleteOptions) []string {
		var uids []string
		for _, d := range deletes {
			if d.Preconditions != nil && d.Preconditions.UID != nil {
				uids = append(uids, string(*d.Preconditions.UID))
			}
		}
		return uids
	}
	if got, want := uidOf(sliceDeletes), []string{string(probeSliceUID)}; !reflect.DeepEqual(got, want) {
		t.Errorf("EndpointSlice DELETEs reaching the server under UID precondition = %v, want %v — a sibling's confirmation must not consume the deadline before this delete is issued", got, want)
	}
	if got, want := uidOf(serviceDeletes), []string{string(probeServiceUID)}; !reflect.DeepEqual(got, want) {
		t.Errorf("Service DELETEs reaching the server under UID precondition = %v, want %v", got, want)
	}
	if got, want := uidOf(nsDeletes), []string{string(originalNsUID)}; !reflect.DeepEqual(got, want) {
		t.Errorf("namespace DELETEs reaching the server under UID precondition = %v, want %v", got, want)
	}
}

// TestCleanNodePortTargetStartsChildrenOnAFreshDeadline: the
// namespace confirmation can spend most of the cleanup deadline
// learning the name is held — a read that answers the replacement
// late — and the children must not inherit the remainder: their
// deletes would be canceled mid-operation, the server unable to
// finish them, both objects left behind with the cleanup reporting
// failure. The child phase starts its own fresh deadline only after
// the replacement is confirmed. Transport-aware for the same reason
// as the sibling-starvation regression (the fake clientset ignores an
// expired context): a real clientset against a local HTTP apiserver
// whose namespace GET confirms the replacement after 800ms of the 1s
// cleanup deadline and whose child DELETEs take 350ms server-side —
// the leftover 200ms cannot cover even one — must remove and confirm
// both children and report success.
func TestCleanNodePortTargetStartsChildrenOnAFreshDeadline(t *testing.T) {
	const (
		nsName           = checkNamespacePrefix + "-cafebabecafe"
		originalNsUID    = types.UID("uid-original-namespace")
		replacementNsUID = types.UID("uid-replaced-namespace")
		probeServiceUID  = types.UID("uid-probe-service")
		probeSliceUID    = types.UID("uid-probe-slice")

		// Of the 1s cleanup deadline the namespace confirmation
		// spends nsConfirmDelay; each child's delete then takes
		// childOpDelay server-side. Children run under the leftover
		// 200ms would have their deletes canceled mid-operation; the
		// fresh child phase covers both children completely.
		nsConfirmDelay = 800 * time.Millisecond
		childOpDelay   = 350 * time.Millisecond
	)
	oldTimeout := nsDeleteTimeout
	nsDeleteTimeout = time.Second
	t.Cleanup(func() { nsDeleteTimeout = oldTimeout })

	var mu sync.Mutex
	var nsDeletes, serviceDeletes, sliceDeletes []metav1.DeleteOptions
	serviceGone, sliceGone := false, false
	writeObject := func(w http.ResponseWriter, obj runtime.Object) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(obj); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
	writeNotFound := func(w http.ResponseWriter, resource schema.GroupResource, name string) {
		status := apierrors.NewNotFound(resource, name).Status()
		status.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(status)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordDelete := func(into *[]metav1.DeleteOptions) {
			var opts metav1.DeleteOptions
			_ = json.NewDecoder(r.Body).Decode(&opts)
			mu.Lock()
			*into = append(*into, opts)
			mu.Unlock()
		}
		// complete finishes one server-side child operation after
		// childOpDelay; a client that disconnects first abandons it,
		// and the object remains — how a starved deadline leaves
		// residue behind.
		complete := func(gone *bool) {
			select {
			case <-time.After(childOpDelay):
				mu.Lock()
				*gone = true
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			case <-r.Context().Done():
			}
		}
		svcPath := "/api/v1/namespaces/" + nsName + "/services/" + nodePortService
		slicePath := "/apis/discovery.k8s.io/v1/namespaces/" + nsName + "/endpointslices/" + nodePortSlice
		switch r.Method + " " + r.URL.Path {
		case "DELETE /api/v1/namespaces/" + nsName:
			recordDelete(&nsDeletes)
			w.WriteHeader(http.StatusOK)
		case "GET /api/v1/namespaces/" + nsName:
			// A different instance holds the name — the children
			// path — answered only after most of the cleanup
			// deadline is spent.
			time.Sleep(nsConfirmDelay)
			writeObject(w, &corev1.Namespace{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
				ObjectMeta: metav1.ObjectMeta{Name: nsName, UID: replacementNsUID},
			})
		case "DELETE " + svcPath:
			recordDelete(&serviceDeletes)
			complete(&serviceGone)
		case "GET " + svcPath:
			mu.Lock()
			gone := serviceGone
			mu.Unlock()
			if gone {
				writeNotFound(w, corev1.Resource("services"), nodePortService)
				return
			}
			writeObject(w, &corev1.Service{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
				ObjectMeta: metav1.ObjectMeta{Name: nodePortService, Namespace: nsName, UID: probeServiceUID},
			})
		case "DELETE " + slicePath:
			recordDelete(&sliceDeletes)
			complete(&sliceGone)
		case "GET " + slicePath:
			mu.Lock()
			gone := sliceGone
			mu.Unlock()
			if gone {
				writeNotFound(w, discoveryv1.Resource("endpointslices"), nodePortSlice)
				return
			}
			writeObject(w, &discoveryv1.EndpointSlice{
				TypeMeta:   metav1.TypeMeta{APIVersion: "discovery.k8s.io/v1", Kind: "EndpointSlice"},
				ObjectMeta: metav1.ObjectMeta{Name: nodePortSlice, Namespace: nsName, UID: probeSliceUID},
			})
		default:
			t.Errorf("unexpected API call: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected call", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	// The clientset speaks JSON for this test's apiserver, like the
	// sibling-starvation regression above (the typed client's default
	// is protobuf, which the UID-precondition assertions decode).
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL, ContentType: "application/json"})
	if err != nil {
		t.Fatalf("building the apiserver client: %v", err)
	}
	target := nodePortTarget{
		peer:   verifiedCluster{name: "p-1"},
		client: cs,
		nsName: nsName,
		nsUID:  originalNsUID,
		marker: "cafebabe",
		svc:    probeChild{kind: "service", name: nodePortService, uid: probeServiceUID, issued: true},
		slice:  probeChild{kind: "endpointslice", name: nodePortSlice, uid: probeSliceUID, issued: true},
	}
	if err := (&Client{}).cleanNodePortTarget(target); err != nil {
		t.Fatalf("cleanNodePortTarget = %v, want nil — the children must run under a fresh deadline, not the namespace confirmation's leftovers", err)
	}
	mu.Lock()
	defer mu.Unlock()
	uidOf := func(deletes []metav1.DeleteOptions) []string {
		var uids []string
		for _, d := range deletes {
			if d.Preconditions != nil && d.Preconditions.UID != nil {
				uids = append(uids, string(*d.Preconditions.UID))
			}
		}
		return uids
	}
	if got, want := uidOf(serviceDeletes), []string{string(probeServiceUID)}; !reflect.DeepEqual(got, want) {
		t.Errorf("Service DELETEs reaching the server under UID precondition = %v, want %v", got, want)
	}
	if got, want := uidOf(sliceDeletes), []string{string(probeSliceUID)}; !reflect.DeepEqual(got, want) {
		t.Errorf("EndpointSlice DELETEs reaching the server under UID precondition = %v, want %v", got, want)
	}
	if got, want := uidOf(nsDeletes), []string{string(originalNsUID)}; !reflect.DeepEqual(got, want) {
		t.Errorf("namespace DELETEs reaching the server under UID precondition = %v, want %v", got, want)
	}
	if !serviceGone || !sliceGone {
		t.Errorf("after the cleanup the service is gone=%v and the endpointslice is gone=%v, want both gone — a canceled delete leaves its object behind", serviceGone, sliceGone)
	}
}

// TestVerifyCrossClusterNodePortReportsUndeterminedChild: a child whose
// create lost its response is resolved at cleanup by one bounded read
// of its name, and a read that cannot say leaves the outcome honestly
// undetermined — the owning line FAILs naming the namespace, the child,
// and the marker a late landing would carry, while the replacement
// holding the name and the service that did commit survive untouched.
func TestVerifyCrossClusterNodePortReportsUndeterminedChild(t *testing.T) {
	const (
		replacementUID  = "uid-replaced-probe-namespace"
		probeServiceUID = "uid-probe-service"
	)
	var marker string
	client, _, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
		if cluster != "p-1" {
			return
		}
		enforceDeleteUID(cs, "namespaces")
		// The service create commits into a namespace a racing
		// replacement takes over — and the response is lost: the
		// child is issued with no UID, the exact ambiguity the
		// attempt marker is for.
		cs.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
			create := action.(k8stesting.CreateAction)
			svc := create.GetObject().(*corev1.Service)
			svc.UID = probeServiceUID
			_ = cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("namespaces"), "", create.GetNamespace())
			_ = cs.Tracker().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: create.GetNamespace(), UID: replacementUID}})
			if err := cs.Tracker().Create(create.GetResource(), svc, svc.GetNamespace()); err != nil {
				return true, nil, err
			}
			return true, nil, errors.New("net/http: request canceled (Client.Timeout exceeded while reading body)")
		})
		// The cleanup's resolving read of that child cannot say what
		// the name holds.
		cs.PrependReactor("get", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
			get := action.(k8stesting.GetAction)
			if get.GetName() != nodePortService || !strings.HasPrefix(get.GetNamespace(), checkNamespacePrefix+"-") {
				return false, nil, nil
			}
			return true, nil, errors.New("read tcp 10.42.0.1:6443->127.0.0.1:5: connection reset by peer")
		})
		// Record the attempt's marker off the namespace create, so the
		// report's ownership advice can be checked against it.
		cs.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
			marker = action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace).Labels[checkAttemptLabelKey]
			return false, nil, nil
		})
	})
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	var p1 CheckResult
	for _, line := range crossClusterLines(results) {
		if line.Status != StatusFail {
			t.Errorf("line for %s = %s (%s), want FAIL (the preparation failed on p-1)", line.Cluster, line.Status, line.Detail)
		}
		if line.Cluster == "p-1" {
			p1 = line
		}
	}
	for _, substr := range []string{
		"could not be determined", "marker", checkAttemptLabelKey, nodePortService, checkNamespacePrefix,
	} {
		if !strings.Contains(p1.Detail, substr) {
			t.Errorf("p-1 line (%s) does not mention %q", p1.Detail, substr)
		}
	}
	if marker != "" && !strings.Contains(p1.Detail, marker) {
		t.Errorf("p-1 line (%s) does not carry this attempt's marker %q", p1.Detail, marker)
	}
	// The replacement survived and is the only prefix namespace on
	// p-1.
	left := probeNamespaces(t, byCluster["p-1"])
	if len(left) != 1 {
		t.Fatalf("prefix namespaces on p-1 = %v, want exactly the replacement", left)
	}
	nsName := left[0]
	ns, err := byCluster["p-1"].CoreV1().Namespaces().Get(ctx, nsName, metav1.GetOptions{})
	if err != nil || string(ns.UID) != replacementUID {
		t.Errorf("replacement namespace = (UID %q, %v), want it untouched with UID %q", ns.UID, err, replacementUID)
	}
	// The service that did commit survives untouched: an undetermined
	// child is reported for a human, never deleted blind. Read it
	// through the fake's storage — the read reactor above errors on
	// purpose.
	obj, err := byCluster["p-1"].Tracker().Get(corev1.SchemeGroupVersion.WithResource("services"), nsName, nodePortService)
	if err != nil {
		t.Fatalf("reading the committed service from the fake's storage: %v", err)
	}
	if svc, ok := obj.(*corev1.Service); !ok || string(svc.UID) != probeServiceUID {
		t.Errorf("committed service = %+v, want it still present under UID %q", obj, probeServiceUID)
	}
	if left := probeNamespaces(t, byCluster["p-2"]); len(left) != 0 {
		t.Errorf("probe namespaces on p-2 = %v, want none", left)
	}
}

// TestVerifyCrossClusterNodePortNamesFailedChildDelete: a child delete
// that fails with a nameless error — the shape a transport or context
// failure has — is reported with the child's exact namespace and name,
// like its confirmation error already was: the line must say what was
// being deleted, not only what kind it was.
func TestVerifyCrossClusterNodePortNamesFailedChildDelete(t *testing.T) {
	const (
		replacementUID  = "uid-replaced-probe-namespace"
		probeServiceUID = "uid-probe-service"
		probeSliceUID   = "uid-probe-slice"
	)
	client, _, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
		if cluster != "p-1" {
			return
		}
		enforceDeleteUID(cs, "namespaces")
		enforceDeleteUID(cs, "services")
		enforceDeleteUID(cs, "endpointslices")
		cs.PrependReactor("create", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
			action.(k8stesting.CreateAction).GetObject().(*corev1.Service).UID = probeServiceUID
			return false, nil, nil
		})
		// The replacement lands at the last child write, so the
		// cleanup must take the children path.
		cs.PrependReactor("create", "endpointslices", func(action k8stesting.Action) (bool, runtime.Object, error) {
			create := action.(k8stesting.CreateAction)
			create.GetObject().(*discoveryv1.EndpointSlice).UID = probeSliceUID
			ns := create.GetNamespace()
			_ = cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("namespaces"), "", ns)
			_ = cs.Tracker().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: replacementUID}})
			return false, nil, nil
		})
		// The EndpointSlice delete fails with a nameless error — no
		// resource, no namespace, no name in it.
		cs.PrependReactor("delete", "endpointslices", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("context deadline exceeded")
		})
	})
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	detail := crossClusterDetail(results, "p-1")
	for _, line := range crossClusterLines(results) {
		switch line.Cluster {
		case "p-1":
			if line.Status != StatusFail ||
				!strings.Contains(line.Detail, "left its probe state behind") ||
				!strings.Contains(line.Detail, "deleting the endpointslice") ||
				!strings.Contains(line.Detail, nodePortSlice) {
				t.Errorf("p-1 line = %s (%s), want the folded cleanup FAIL naming the failed child delete", line.Status, line.Detail)
			}
		case "p-2":
			if line.Status != StatusPass {
				t.Errorf("p-2 line = %s (%s), want PASS (its own cleanup succeeded)", line.Status, line.Detail)
			}
		}
	}
	// The replacement is the only prefix namespace left on p-1, and
	// its name is the namespace the failed delete must carry.
	left := probeNamespaces(t, byCluster["p-1"])
	if len(left) != 1 {
		t.Fatalf("prefix namespaces on p-1 = %v, want exactly the replacement", left)
	}
	nsName := left[0]
	if !strings.Contains(detail, "endpointslice "+nsName+"/"+nodePortSlice) {
		t.Errorf("p-1 line (%s) must name the child as %s/%s", detail, nsName, nodePortSlice)
	}
	// The sibling child was still attempted and removed.
	if _, err := byCluster["p-1"].CoreV1().Services(nsName).Get(ctx, nodePortService, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("reading the check's service: %v, want NotFound (attempted despite the sibling's failed delete)", err)
	}
}

// crossClusterDetail returns one cluster's cross-cluster line detail.
func crossClusterDetail(results []CheckResult, cluster string) string {
	for _, line := range crossClusterLines(results) {
		if line.Cluster == cluster {
			return line.Detail
		}
	}
	return ""
}

// TestVerifyCrossClusterNodePortRefusesWritingIntoReplacedNamespace:
// the UID re-check before each child write catches the replacement the
// check can see: here the namespace is replaced between its creation
// and the Service create, so the re-read that precedes the write
// observes the replacement's UID and refuses — no child is ever
// created, and the replacement survives untouched.
func TestVerifyCrossClusterNodePortRefusesWritingIntoReplacedNamespace(t *testing.T) {
	const replacementUID = "uid-replaced-probe-namespace"
	client, _, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
		if cluster != "p-1" {
			return
		}
		enforceDeleteUID(cs, "namespaces")
		// The out-of-band replacement, landing between the namespace's
		// creation and the re-read that precedes the first child
		// write: the very next read of the name observes it.
		cs.PrependReactor("get", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
			name := action.(k8stesting.GetAction).GetName()
			if !strings.HasPrefix(name, checkNamespacePrefix+"-") {
				return false, nil, nil
			}
			_ = cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("namespaces"), "", name)
			_ = cs.Tracker().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: replacementUID}})
			return true, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: replacementUID}}, nil
		})
	})
	ctx := context.Background()
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, line := range crossClusterLines(results) {
		if line.Status != StatusFail || !strings.Contains(line.Detail, "refusing to write the check's objects into the replacement") {
			t.Errorf("line for %s = %s (%s), want FAIL refusing the write into the replacement", line.Cluster, line.Status, line.Detail)
		}
	}
	// No child was ever written on p-1: the refusal preceded both
	// creates.
	for _, a := range byCluster["p-1"].Actions() {
		if a.GetVerb() == "create" && (a.GetResource().Resource == "services" || a.GetResource().Resource == "endpointslices") {
			t.Errorf("p-1 saw %s %s; the re-check must refuse before any child is written", a.GetVerb(), a.GetResource().Resource)
		}
	}
	// The replacement survived, and it is the only prefix namespace on
	// p-1.
	left := probeNamespaces(t, byCluster["p-1"])
	if len(left) != 1 {
		t.Fatalf("prefix namespaces on p-1 = %v, want exactly the replacement", left)
	}
	ns, err := byCluster["p-1"].CoreV1().Namespaces().Get(ctx, left[0], metav1.GetOptions{})
	if err != nil || string(ns.UID) != replacementUID {
		t.Errorf("replacement namespace = (%q, %v), want it untouched with UID %q", ns.UID, err, replacementUID)
	}
}

// metaOf adapts a runtime object from the tracker to its metadata.
func metaOf(obj runtime.Object) (metav1.Object, error) {
	meta, ok := obj.(metav1.Object)
	if !ok {
		return nil, fmt.Errorf("object %T carries no metadata", obj)
	}
	return meta, nil
}

// enforceDeleteUID makes the fake apiserver apply a delete's UID
// precondition the way the real one does: a live object whose UID
// differs from the precondition's is a Conflict, never a deletion. The
// fake tracker ignores preconditions entirely, which is why every
// ownership-proof regression installs this for the resource its delete
// targets.
func enforceDeleteUID(cs *fake.Clientset, resource string) {
	cs.PrependReactor("delete", resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		del := action.(k8stesting.DeleteAction)
		pre := del.GetDeleteOptions().Preconditions
		if pre == nil || pre.UID == nil {
			return false, nil, nil
		}
		live, err := cs.Tracker().Get(del.GetResource(), del.GetNamespace(), del.GetName())
		if err != nil {
			return false, nil, nil // not present: the default reactor answers NotFound
		}
		liveMeta, err := metaOf(live)
		if err != nil {
			return false, nil, nil
		}
		if string(liveMeta.GetUID()) != string(*pre.UID) {
			return true, nil, apierrors.NewConflict(
				del.GetResource().GroupResource(), del.GetName(),
				fmt.Errorf("uid %s does not match the live %s", *pre.UID, liveMeta.GetUID()))
		}
		return false, nil, nil
	})
}

// TestVerifyCrossClusterNodePortResolvesAmbiguousCreate: a namespace
// create that errors is not proof nothing was created — the server can
// persist the namespace and lose the response, and client-go's own
// retry can commit one attempt and surface the next one's refusal —
// so the attempt is settled by its marker on a follow-up read, and the
// failure names the namespace in every branch.
func TestVerifyCrossClusterNodePortResolvesAmbiguousCreate(t *testing.T) {
	t.Run("committed despite the lost response", func(t *testing.T) {
		const committedUID = "uid-committed-probe-namespace"
		client, _, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
			if cluster != "p-1" {
				return
			}
			enforceDeleteUID(cs, "namespaces")
			cs.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
				create := action.(k8stesting.CreateAction)
				ns := create.GetObject().(*corev1.Namespace)
				ns.UID = committedUID
				// The server persists the create...
				if err := cs.Tracker().Create(create.GetResource(), ns, ns.GetNamespace()); err != nil {
					return true, nil, err
				}
				// ...and the response is lost.
				return true, nil, errors.New("net/http: request canceled (Client.Timeout exceeded while reading body)")
			})
		})
		ctx := context.Background()
		if _, err := client.Create(ctx); err != nil {
			t.Fatalf("create: %v", err)
		}
		results, err := client.Verify(ctx, nil)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		for _, line := range crossClusterLines(results) {
			if line.Status != StatusFail || !strings.Contains(line.Detail, "the create had in fact committed") {
				t.Errorf("line for %s = %s (%s), want FAIL reporting the committed create", line.Cluster, line.Status, line.Detail)
			}
			if !strings.Contains(line.Detail, checkNamespacePrefix) {
				t.Errorf("failure must name the namespace: %s", line.Detail)
			}
		}
		// The committed namespace was removed under the UID the
		// resolving read observed — the enforcing reactor turns a
		// delete by any other UID into a Conflict, so a clean board
		// proves the observed UID was the precondition.
		if left := probeNamespaces(t, byCluster["p-1"]); len(left) != 0 {
			t.Errorf("probe namespaces left on p-1 = %v, want none (the committed create is cleaned by its observed UID)", left)
		}
	})

	t.Run("a foreign holder of the name is never deleted", func(t *testing.T) {
		const foreignUID = "uid-foreign-namespace"
		client, _, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
			if cluster != "p-1" {
				return
			}
			enforceDeleteUID(cs, "namespaces")
			cs.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
				name := action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace).Name
				// A namespace this check did not create, holding the
				// very name the attempt generated — the collision the
				// random name makes vanishingly rare, forced here,
				// which is also the shape a name held before the
				// create ever ran has.
				_ = cs.Tracker().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: foreignUID}})
				return true, nil, apierrors.NewAlreadyExists(corev1.Resource("namespaces"), name)
			})
		})
		ctx := context.Background()
		if _, err := client.Create(ctx); err != nil {
			t.Fatalf("create: %v", err)
		}
		results, err := client.Verify(ctx, nil)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		for _, line := range crossClusterLines(results) {
			if line.Status != StatusFail || !strings.Contains(line.Detail, "left alone") {
				t.Errorf("line for %s = %s (%s), want FAIL reporting the foreign holder", line.Cluster, line.Status, line.Detail)
			}
			if !strings.Contains(line.Detail, checkNamespacePrefix) {
				t.Errorf("failure must name the namespace: %s", line.Detail)
			}
		}
		left := probeNamespaces(t, byCluster["p-1"])
		if len(left) != 1 {
			t.Fatalf("prefix namespaces on p-1 = %v, want exactly the foreign holder", left)
		}
		ns, err := byCluster["p-1"].CoreV1().Namespaces().Get(ctx, left[0], metav1.GetOptions{})
		if err != nil || string(ns.UID) != foreignUID {
			t.Errorf("foreign holder = (%q, %v), want it untouched with UID %q", ns.UID, err, foreignUID)
		}
	})

	t.Run("not found: the create did not take effect", func(t *testing.T) {
		client, _, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
			if cluster != "p-1" {
				return
			}
			cs.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("connection reset by peer")
			})
		})
		ctx := context.Background()
		if _, err := client.Create(ctx); err != nil {
			t.Fatalf("create: %v", err)
		}
		results, err := client.Verify(ctx, nil)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		for _, line := range crossClusterLines(results) {
			if line.Status != StatusFail || !strings.Contains(line.Detail, "did not take effect") {
				t.Errorf("line for %s = %s (%s), want FAIL reporting the create that did not take effect", line.Cluster, line.Status, line.Detail)
			}
		}
		if left := probeNamespaces(t, byCluster["p-1"]); len(left) != 0 {
			t.Errorf("probe namespaces on p-1 = %v, want none", left)
		}
	})

	t.Run("an unreadable outcome is reported as undetermined", func(t *testing.T) {
		client, _, byCluster := nodePortTestClient(t, func(cs *fake.Clientset, cluster string) {
			if cluster != "p-1" {
				return
			}
			cs.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("connection reset by peer")
			})
			cs.PrependReactor("get", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if !strings.HasPrefix(action.(k8stesting.GetAction).GetName(), checkNamespacePrefix+"-") {
					return false, nil, nil
				}
				return true, nil, errors.New("read tcp 10.42.0.1:6443->127.0.0.1:5: connection reset by peer")
			})
		})
		ctx := context.Background()
		if _, err := client.Create(ctx); err != nil {
			t.Fatalf("create: %v", err)
		}
		results, err := client.Verify(ctx, nil)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		for _, line := range crossClusterLines(results) {
			if line.Status != StatusFail || !strings.Contains(line.Detail, "could not be determined") {
				t.Errorf("line for %s = %s (%s), want FAIL reporting the undetermined outcome", line.Cluster, line.Status, line.Detail)
			}
			if !strings.Contains(line.Detail, checkNamespacePrefix) || !strings.Contains(line.Detail, "marker") {
				t.Errorf("failure must name the namespace and the marker for a late landing: %s", line.Detail)
			}
		}
		if left := probeNamespaces(t, byCluster["p-1"]); len(left) != 0 {
			t.Errorf("probe namespaces on p-1 = %v, want none", left)
		}
	})
}

// TestApiserverEndpoint pins the endpoint extraction's validation: both
// fields the connect needs are optional in discovery/v1, so unusable
// candidates are skipped and exhaustion is an error, never a panic or a
// zero-value endpoint handed to a connect.
func TestApiserverEndpoint(t *testing.T) {
	slice := func(name string, ports []discoveryv1.EndpointPort, addrs ...string) *discoveryv1.EndpointSlice {
		return &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Endpoints:  []discoveryv1.Endpoint{{Addresses: addrs}},
			Ports:      ports,
		}
	}
	httpsPort := func(port int32) []discoveryv1.EndpointPort {
		return []discoveryv1.EndpointPort{{Name: ptrTo("https"), Protocol: ptrTo(corev1.ProtocolTCP), Port: ptrTo(port)}}
	}

	t.Run("a usable slice yields its address and port", func(t *testing.T) {
		addr, port, err := apiserverEndpoint(&discoveryv1.EndpointSliceList{
			Items: []discoveryv1.EndpointSlice{*slice("kubernetes", httpsPort(6443), "10.42.0.1")},
		})
		if err != nil {
			t.Fatalf("apiserverEndpoint = %v, want success", err)
		}
		if addr.String() != "10.42.0.1" || port != 6443 {
			t.Errorf("endpoint = (%s, %d), want (10.42.0.1, 6443)", addr, port)
		}
	})

	t.Run("a nil port is skipped for the next candidate", func(t *testing.T) {
		nilPort := slice("half-formed", []discoveryv1.EndpointPort{{Name: ptrTo("https"), Port: nil}}, "10.42.0.9")
		usable := slice("kubernetes", httpsPort(6443), "10.42.0.1")
		addr, port, err := apiserverEndpoint(&discoveryv1.EndpointSliceList{Items: []discoveryv1.EndpointSlice{*nilPort, *usable}})
		if err != nil {
			t.Fatalf("apiserverEndpoint = %v, want the usable slice to win", err)
		}
		if addr.String() != "10.42.0.1" || port != 6443 {
			t.Errorf("endpoint = (%s, %d), want the usable slice's (10.42.0.1, 6443)", addr, port)
		}
	})

	t.Run("ports with no values everywhere is an error", func(t *testing.T) {
		_, _, err := apiserverEndpoint(&discoveryv1.EndpointSliceList{
			Items: []discoveryv1.EndpointSlice{*slice("half-formed", []discoveryv1.EndpointPort{{Name: ptrTo("https"), Port: nil}}, "10.42.0.1")},
		})
		if err == nil || !strings.Contains(err.Error(), "ports with no values") {
			t.Fatalf("apiserverEndpoint = %v, want an error naming the valueless ports", err)
		}
	})

	t.Run("a non-IP address is an error naming it", func(t *testing.T) {
		_, _, err := apiserverEndpoint(&discoveryv1.EndpointSliceList{
			Items: []discoveryv1.EndpointSlice{*slice("kubernetes", httpsPort(6443), "1.2.3.4; touch /tmp/pwned")},
		})
		if err == nil || !strings.Contains(err.Error(), "is not an IP") {
			t.Fatalf("apiserverEndpoint = %v, want an error naming the unparseable address", err)
		}
	})

	t.Run("no slices at all is an error", func(t *testing.T) {
		_, _, err := apiserverEndpoint(&discoveryv1.EndpointSliceList{})
		if err == nil {
			t.Fatal("apiserverEndpoint(empty) = nil, want an error")
		}
	})
}

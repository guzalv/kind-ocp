package kindocp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/guzalv/kind-ocp/internal/kube"
	"github.com/guzalv/kind-ocp/provider"
)

// The cross-cluster NodePort reachability check (AC-1.4, AC-5.1): the
// Central-to-Sensor pattern needs a pod on one cluster to reach a
// NodePort service on another by node IP, which works only because
// every kind cluster attaches to the same shared `kind` Docker network.
//
// What the check runs, per ordered pair (source A, target B):
//
//   - On B, through the Kubernetes API: a namespace holding a
//     selectorless Service of type NodePort plus a hand-written
//     EndpointSlice pointing at the endpoint the cluster itself uses
//     for its apiserver — the control-plane node's IP (read from the
//     default `kubernetes` Endpoints) and the port the provisioner
//     itself configured that apiserver to listen on. Kube-proxy
//     programs the NodePort like any other, so the connect exercises
//     the real service path, and the apiserver is the listener — a
//     process that exists on every cluster regardless of profile, so
//     the check never pulls an image and never depends on a layer.
//   - On A, through the provisioner's TCP-probe capability
//     (provider.TCPProber): a connect from A's control-plane node to
//     B's node IP and the allocated node port.
//
// The endpoint the check points its Service and its connect at is
// corroborated, never trusted from the cluster's API alone. The
// objects that report it inside B are writable by whoever can edit
// them, so a stale or tampered endpoint could name an unrelated
// listener and the check would report a PASS that tested nothing —
// the probe establishes that a listener answered, never which one,
// nor on which node. Before any probe object exists, the endpoint
// read from B's API — the whole endpoint, address and port together —
// must be one of the control-plane API endpoints the provisioner
// itself reports for B (provider.ControlPlaneAPIEndpointResolver):
// the runtime's own knowledge of where the node lives on the shared
// network and what port it configured the apiserver to listen on
// there, which no in-cluster edit rewrites. The matched endpoint is
// also the only backend the check ever writes: whoever can edit the
// reporting objects could otherwise select both the node and the
// listener the check publishes a NodePort to. A provisioner without
// the capability leaves the whole check skipped with that reason: an
// uncorroborated reachability answer is exactly what the
// corroboration exists to make impossible, so it is never produced.
//
// The connect runs from A's node container — the network namespace
// every pod's off-cluster traffic is masqueraded through — because no
// image preloaded on a kind node carries a connect tool (kube-proxy,
// coredns, etcd, and the rest are distroless; verified against
// kindest/node v1.36.4), so a pod-based client would force a registry
// pull into a check that must also run on minimal-profile clusters.
// What it proves is the property the shared network exists for: A and B
// are on one network, and B's kube-proxy routes B's NodePort to a
// listening endpoint. The pod-network half of the pattern — a workload
// pod on A reaching B — is proven by the integration test with a real
// client pod, and end to end by the StackRox example (increment 9).
//
// Ownership of the probe's namespace is proven, never assumed. The
// namespace's name is unique to one preparation (a fresh random tail
// every run), so the probe never writes into a namespace another run —
// or anyone else — created, and two verifies running at once never
// share one. Every preparation also stamps its namespace with a
// per-attempt ownership marker (a label only that attempt's create
// ever set), because a create that errors is not proof nothing was
// created: the server can persist the namespace and lose the response
// (the setup deadline expiring mid-body, a connection reset past the
// write), and client-go's own retry can turn a committed first
// attempt into an AlreadyExists. An ambiguous create is settled by one
// follow-up read of the name under the marker: a namespace carrying
// the marker is this attempt's — the preparation fails, but the
// namespace is registered for cleanup under the UID the read observed
// — while a namespace without it is not this check's and is left
// entirely alone. The namespace's UID is required as the deletion
// precondition: a namespace replaced under the same name is not this
// check's to remove, and the API refuses the delete instead of taking
// the replacement. That refusal is not assumed to mean one, and no
// deletion's return is taken as proof of absence: the API answers an
// admission refusal with the same 409 a replacement's UID mismatch
// produces, and an accepted delete returns while the namespace
// controller still drains the contents. Every removal is confirmed
// by reading the name under the cleanup's own budget — NotFound or a
// different UID proves the instance the check created is gone — and
// only a name held by a different instance turns to the children.
//
// The children get the same ownership proof as the namespace. Before
// each child write the namespace is re-read and confirmed still the
// created instance (a replacement is refused, never written into),
// and each child is stamped with the attempt's marker. The race
// between a re-check and the write after it cannot be closed by a
// check; a child that lands in a replacement is removed by its own
// UID while the replacement and everything else in it survives, and
// a child whose create lost its response (committed and errored)
// carries no UID — the cleanup resolves it by the marker on one
// bounded read of its name and deletes only a marker-matched child,
// under the UID the read observed. The children run in their own phase
// under a fresh deadline, started only once the name is confirmed
// held by another instance — the namespace confirmation before it can
// spend most of the cleanup's first deadline, and deletes issued
// under the remainder would be canceled mid-operation — and within
// that phase both children are cleaned at the same time, each from its
// start: a child whose confirmation polls to the deadline's end must
// not starve the other's delete, and both are attempted however
// either goes. A namespace left by a killed run is
// never adopted or removed — the unique name cannot collide with it,
// and a delete by name alone would remove state this run cannot
// prove it owns; deleting the cluster removes such residue with
// everything else.
//
// Like every layer check, this check leaves no state behind: the
// namespace is deleted and its deletion confirmed on every path — a
// preparation that fails after creating its namespace still returns
// it, and the cleanup covers partial preparations exactly like
// completed ones — and a cleanup failure FAILs the owning cluster's
// line, folded into that line's verdict, never printed as a second
// line after a PASS. Cleanup runs under its own bounded context, so a
// cancelled or expired check still removes what it created.
const (
	// crossClusterCheck is the check's name in verify output.
	crossClusterCheck = "cross-cluster-nodeport"

	// checkNamespacePrefix starts the probe namespace's name:
	// kind-ocp-verify-<random>. The prefix is the residue signature a
	// human can recognize; the random tail is the uniqueness that keeps
	// the probe out of everyone else's namespaces — including its own
	// earlier runs'.
	checkNamespacePrefix = "kind-ocp-verify"

	// checkAttemptLabelKey carries the per-attempt ownership marker on
	// the probe's namespace. The name keeps runs apart; the marker is
	// what settles an ambiguous create, because only this attempt's
	// create ever stamped it: a namespace carrying it is this
	// attempt's whatever the create's error said, and a namespace
	// without it never is.
	checkAttemptLabelKey = "nodeport.kind-ocp.io/probe-attempt"

	// nodePortService and nodePortSlice name the Service and its
	// EndpointSlice inside the probe namespace.
	nodePortService = "nodeport-check"
	nodePortSlice   = "nodeport-check"
)

// Budgets for the check's waits and calls. Variables, not constants, so
// the regression tests can drive them short (the stuck-namespace
// failure mode must not cost a minute in a unit test); production
// always sees the defaults. Every API call and wait must be bounded
// (AC-6.5), and no documented use case overrides them through
// configuration.
var (
	// nsDeleteTimeout bounds each namespace-deletion confirmation.
	nsDeleteTimeout = 60 * time.Second

	// connectTimeout bounds one connect attempt, which itself retries
	// for about twenty seconds inside the node (kube-proxy needs a
	// beat to program a freshly created EndpointSlice).
	connectTimeout = 60 * time.Second

	// setupTimeout bounds the check's setup on one target: the identity
	// read, the endpoint read, and the three creates. A stalled
	// apiserver response must fail the check, not hang verify.
	setupTimeout = 30 * time.Second
)

// verifiedCluster is what verifyCluster hands the cross-cluster check:
// one cluster it selected, with the kubeconfig it exported and the
// instance identity its record check validated — empty when the cluster
// did not verify (running, record found, and the record bound to the
// live instance). Only verified clusters take part in the check — a
// peer that failed its identity check is reported by its own lines, not
// by a reachability failure it did not cause.
type verifiedCluster struct {
	name           string
	kubeconfigPath string
	instanceID     string
}

// nodePortTarget is one prepared target: a peer carrying a NodePort
// service the check can connect to, plus everything the cleanup of the
// probe's residue needs — the client the namespace was created
// through (never a re-load of the kubeconfig path, which could point
// somewhere else by then), the namespace's UID (the ownership proof
// the deletion requires), the attempt's ownership marker (the one
// value only this preparation's creates ever stamped, on the
// namespace and on both children — what settles a child whose create
// lost its response), and one probeChild per object written inside
// the namespace, each carrying how far its create got. A target whose
// preparation failed midway carries the namespace fields and
// whichever children it got to; that is the residue registration, and
// the caller cleans it like a completed target. A preparation refused
// before the namespace exists — an unreadable endpoint, an
// uncorroborated one — carries none of that, because it created
// nothing to register.
type nodePortTarget struct {
	peer     verifiedCluster
	client   kubernetes.Interface
	nsName   string
	nsUID    types.UID
	marker   string
	svc      probeChild
	slice    probeChild
	nodeAddr netip.Addr
	nodePort int32
}

// probeChild is one object the check writes inside its namespace: how
// to name it in cleanup reports, and how far its create got. uid is
// what the create returned when its response arrived; issued with an
// empty uid means the create was issued but its response was lost —
// whether it committed is exactly what the attempt marker settles at
// cleanup.
type probeChild struct {
	kind   string
	name   string
	uid    types.UID
	issued bool
}

// checkCrossClusterNodePort runs the cross-cluster NodePort
// reachability check over the clusters verify selected. It emits
// exactly one cross-cluster-nodeport line per cluster: PASS with the
// endpoints reached, FAIL naming the pair that could not connect (or
// the state it left behind), or SKIP with the reason it did not run —
// an unverified cluster, a single-cluster prefix, or a provisioner
// without the TCP-probe or the API-endpoint capability. The
// corroboration capability is as required as the connect: without it
// the endpoint cannot be corroborated against the node and listener
// the provisioner itself configured, and the check never runs
// uncorroborated.
func (c *Client) checkCrossClusterNodePort(ctx context.Context, clusters []verifiedCluster) []CheckResult {
	// Eligibility: only clusters that verified against their record
	// take part, as sources or as targets.
	var eligible []verifiedCluster
	for _, cl := range clusters {
		if cl.instanceID != "" {
			eligible = append(eligible, cl)
		}
	}

	prober, hasProber := c.prov.(provider.TCPProber)
	resolver, hasResolver := c.prov.(provider.ControlPlaneAPIEndpointResolver)
	if !hasProber || !hasResolver || len(eligible) < 2 {
		// Nothing will run. Every cluster still gets its line, with
		// the reason that applies to it: a cluster that did not
		// verify is reported by its own checks; an eligible cluster
		// with no peer has nothing to reach (the more fundamental
		// reason — with one cluster no capability matters); only a
		// peer-carrying prefix names the missing capability, and a
		// missing API-endpoint capability is reported like a missing
		// TCP-probe one — a question the provisioner cannot answer,
		// not one it answered badly.
		results := make([]CheckResult, 0, len(clusters))
		for _, cl := range clusters {
			result := CheckResult{Cluster: cl.name, Check: crossClusterCheck, Status: StatusSkipped}
			switch {
			case cl.instanceID == "":
				result.Detail = "cluster not verified against its record (see its checks above); reachability not checked"
			case len(eligible) < 2:
				result.Detail = "no other verified cluster in this prefix; the check needs at least two clusters"
			case !hasProber:
				result.Detail = "the provisioner cannot connect from cluster nodes (no TCP-probe capability); reachability not checked"
			default:
				result.Detail = "the provisioner cannot report its clusters' control-plane API endpoints (no API-endpoint capability); " +
					"an endpoint read from a cluster's API cannot be corroborated against the node and listener the provisioner itself configured, so reachability not checked"
			}
			results = append(results, result)
		}
		return results
	}

	// Prepare every target first: the services the connects go to. A
	// preparation that fails after creating its namespace still returns
	// the target — the namespace is the check's residue from the moment
	// it exists — so one tracked slice carries completed and partial
	// targets alike and the cleanup below covers both on every path;
	// any preparation error already suppresses every connect below, so
	// the connects never see a partial target.
	targets := make([]nodePortTarget, 0, len(eligible))
	var prepErrs []error
	for _, peer := range eligible {
		target, err := c.prepareNodePortTarget(ctx, peer, resolver)
		if err != nil {
			prepErrs = append(prepErrs, fmt.Errorf("preparing the NodePort service on %s: %w", peer.name, err))
		}
		if target.nsName != "" {
			targets = append(targets, target)
		}
	}

	// Attribute one line per source. A target that could not be
	// prepared fails every source's line — the sources never reached a
	// peer they share a network with, and reporting a pass over the
	// prepared half would hide the missing one — and no connect runs:
	// the check does not spend its probes on a run already failed. The
	// lines are composed after cleanup, so that one cluster's line
	// carries one verdict: its connects and its own residue, never a
	// PASS for the check and a FAIL for its cleanup.
	results := make([]CheckResult, 0, len(eligible))
	for _, source := range eligible {
		result := CheckResult{Cluster: source.name, Check: crossClusterCheck, Status: StatusPass}
		switch {
		case len(prepErrs) > 0:
			result.Status = StatusFail
			result.Detail = joinErrText(prepErrs)
		default:
			for _, target := range targets {
				if target.peer.name == source.name {
					continue
				}
				if err := c.reachNodePort(ctx, prober, source.name, target); err != nil {
					result.Status = StatusFail
					result.Detail = fmt.Sprintf("reaching %s: %v", target.peer.name, err)
					break
				}
			}
			if result.Status == StatusPass {
				result.Detail = describeTargets(source.name, targets)
			}
		}
		results = append(results, result)
	}

	// Cleanup: remove the probe namespace from every target, completed
	// or partial, and confirm it gone. It runs under its own bounded
	// context — a cancelled check still cleans up after itself — and a
	// failure here FAILs the owning cluster's line, folded into the
	// verdict that line already carries.
	cleanupErrs := make(map[string]error, len(eligible))
	for _, target := range targets {
		if err := c.cleanNodePortTarget(target); err != nil {
			cleanupErrs[target.peer.name] = err
		}
	}
	for i := range results {
		err, ok := cleanupErrs[results[i].Cluster]
		if !ok {
			continue
		}
		if results[i].Status == StatusPass {
			results[i].Status = StatusFail
			results[i].Detail = fmt.Sprintf("the check left its probe state behind on this cluster: %v", err)
		} else {
			results[i].Detail = fmt.Sprintf("%s; cleaning up the probe state: %v", results[i].Detail, err)
		}
	}
	return results
}

// describeTargets renders the PASS detail: every endpoint the source
// reached.
func describeTargets(source string, targets []nodePortTarget) string {
	out := ""
	for _, target := range targets {
		if target.peer.name == source {
			continue
		}
		if out != "" {
			out += ", "
		}
		out += fmt.Sprintf("%s (node %s:%d)", target.peer.name, target.nodeAddr, target.nodePort)
	}
	return "reached NodePort on " + out
}

func joinErrText(errs []error) string {
	out := ""
	for _, err := range errs {
		if out != "" {
			out += "; "
		}
		out += err.Error()
	}
	return out
}

// prepareNodePortTarget creates the probe's own namespace and, inside
// it, the selectorless NodePort Service plus the EndpointSlice pointing
// at the cluster's own apiserver endpoint. It returns the node address
// and the allocated node port a connect should use.
//
// Before anything is created, the endpoint read from the cluster's API
// is corroborated as a whole — address and port together — against the
// control-plane API endpoints the provisioner reports for the cluster
// (resolver), and the matched endpoint is the only backend the check
// writes: the objects reporting that endpoint inside the cluster are
// writable by whoever can edit them, so an unchecked one would let
// whoever edits them select both the node and the listener the check
// publishes a NodePort to, while the probe's answer (a TCP connect
// succeeded) can name neither. A mismatch is a refusal with nothing
// created and nothing dialed — never a probe pointed at an endpoint
// the check cannot attribute.
//
// The mutation stays bound to the cluster instance verify validated:
// the client built here is the client the namespace is created through,
// the live identity is read through that same client and must equal the
// recorded one, and the same client is retained on the target for
// cleanup — a kubeconfig replaced between the record check and this
// preparation is a refusal, never a mutation of whatever the
// replacement points at.
//
// Every failure after the namespace exists returns the target anyway
// (the caller's cleanup removes it on every path), and every setup call
// is bounded by one deadline (AC-6.5).
func (c *Client) prepareNodePortTarget(ctx context.Context, peer verifiedCluster, resolver provider.ControlPlaneAPIEndpointResolver) (nodePortTarget, error) {
	cs, err := c.clientset(peer.kubeconfigPath)
	if err != nil {
		return nodePortTarget{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()

	// The kubeconfig was exported and the record checked moments ago.
	// This client is the one the mutation runs through, so the identity
	// is re-read through it: a kubeconfig swapped to a same-name
	// replacement in between shows a different instance here.
	live, err := kube.ClusterInstanceID(ctx, cs)
	if err != nil {
		return nodePortTarget{}, fmt.Errorf("reading the live cluster identity: %w", err)
	}
	if live != peer.instanceID {
		return nodePortTarget{}, fmt.Errorf(
			"the kubeconfig for %s now points at a different cluster instance than the one verify validated "+
				"(it was probably replaced after the identity check) — refusing to prepare a NodePort target on it",
			peer.name)
	}

	// The cluster's own apiserver endpoint: the default kubernetes
	// service's EndpointSlices carry the control-plane node's IP and
	// the apiserver port — a listener that exists on every cluster and
	// the address the cluster itself routes to.
	slices, err := cs.DiscoveryV1().EndpointSlices("default").List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=kubernetes",
	})
	if err != nil {
		return nodePortTarget{}, fmt.Errorf("reading the apiserver endpoint: %w", err)
	}
	addr, port, err := apiserverEndpoint(slices)
	if err != nil {
		return nodePortTarget{}, err
	}

	// The endpoint is corroborated as a whole against the
	// provisioner's own record of where this cluster's apiserver
	// listens, before any probe object exists: an endpoint the
	// cluster's own API reports is writable inside the cluster, so a
	// stale or tampered one naming another listener — or another node
	// — is refused here, and the Service backend and the connect never
	// see it. The refusal created nothing to clean, and the backend
	// the check does write is the matched, provisioner-owned endpoint.
	backend, err := corroborateAPIEndpoint(ctx, resolver, peer.name, addr, port)
	if err != nil {
		return nodePortTarget{}, err
	}

	attempt, err := newCheckNamespaceAttempt()
	if err != nil {
		return nodePortTarget{}, err
	}
	nsName := attempt.name
	ns, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   attempt.name,
			Labels: map[string]string{checkAttemptLabelKey: attempt.marker},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		// A failed create is not proof nothing was created: the
		// request can have committed before the failure, and only the
		// attempt's marker settles what the name now holds. The name
		// is never adopted on the error's say-so and never abandoned
		// on it either.
		return resolveCheckNamespaceCreate(cs, peer, attempt, err)
	}
	target := attempt.baseTarget(peer, cs, ns.UID)

	// The namespace is confirmed still the instance the create
	// returned before anything is written into it: a namespace
	// replaced under the same name in between would otherwise receive
	// the check's Service and EndpointSlice, and the UID-preconditioned
	// delete could never clean them out of the replacement. The window
	// between this read and the write after it cannot be closed by a
	// check; the children's marker and UIDs are what cover it at
	// cleanup.
	if err := stillOurNamespace(ctx, cs, attempt.name, ns.UID); err != nil {
		return target, err
	}
	// The Service carries the attempt marker like the namespace does:
	// a child create can lose its response the same way (commit and
	// error), and the marker is what settles, at cleanup, whether the
	// object under this name is this check's.
	target.svc.issued = true
	svc, err := cs.CoreV1().Services(nsName).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodePortService,
			Namespace: nsName,
			Labels:    map[string]string{checkAttemptLabelKey: attempt.marker},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{
				Name:     "check",
				Protocol: corev1.ProtocolTCP,
				Port:     443,
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return target, fmt.Errorf("creating the NodePort service: %w", err)
	}
	target.svc.uid = svc.UID
	var nodePort int32
	if len(svc.Spec.Ports) > 0 {
		nodePort = svc.Spec.Ports[0].NodePort
	}
	if nodePort == 0 {
		return target, fmt.Errorf("the NodePort service %s/%s reports no allocated node port", nsName, nodePortService)
	}

	if err := stillOurNamespace(ctx, cs, attempt.name, ns.UID); err != nil {
		return target, err
	}
	addressType := discoveryv1.AddressTypeIPv4
	if !backend.Addr().Is4() {
		addressType = discoveryv1.AddressTypeIPv6
	}
	portName := "check"
	protocol := corev1.ProtocolTCP
	// The backend written is the matched, provisioner-owned endpoint —
	// never the cluster-read values as such: the corroboration above
	// proved the two equal, and writing the provisioner's keeps the
	// published backend an endpoint the runtime configured — not one
	// an endpoint writer selected, which the probe could not tell
	// apart from the apiserver (a TCP connect names no listener).
	portVal := int32(backend.Port())
	target.slice.issued = true
	slice, err := cs.DiscoveryV1().EndpointSlices(nsName).Create(ctx, &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodePortSlice,
			Namespace: nsName,
			// The label kube-proxy resolves slices by, and the
			// attempt marker for the cleanup's ownership resolution.
			Labels: map[string]string{
				discoveryv1.LabelServiceName: nodePortService,
				checkAttemptLabelKey:         attempt.marker,
			},
		},
		AddressType: addressType,
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{backend.Addr().String()}},
		},
		Ports: []discoveryv1.EndpointPort{
			{Name: &portName, Protocol: &protocol, Port: &portVal},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return target, fmt.Errorf("creating the check's EndpointSlice: %w", err)
	}
	target.slice.uid = slice.UID

	target.nodeAddr, target.nodePort = backend.Addr(), nodePort
	c.log.Info("prepared cross-cluster NodePort check target",
		"cluster", peer.name, "namespace", nsName, "node", backend.Addr().String(), "nodePort", nodePort)
	return target, nil
}

// checkNamespaceAttempt is one preparation's namespace identity: the
// unique name the probe writes under, and the per-attempt ownership
// marker its create stamps onto the namespace. The name keeps
// concurrent runs and killed-run leftovers apart; the marker is what
// settles an ambiguous create, because only this attempt's create ever
// set it — ownership of a resolved namespace is decided by a value
// only this attempt stamped, never by the name it happened to use.
type checkNamespaceAttempt struct {
	name   string
	marker string
}

// newCheckNamespaceAttempt returns the probe namespace's identity:
// kind-ocp-verify-<12 hex chars> for the name, and a fresh random
// marker for the ownership label. Uniqueness of the name is half the
// ownership story — the probe never writes into a namespace another
// run or anyone else created — and the marker is the other half: it
// distinguishes this attempt's namespace from a foreign or replaced
// holder of the same name when a create's outcome is unknown.
func newCheckNamespaceAttempt() (checkNamespaceAttempt, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on the platforms kind-ocp runs on;
		// a refusal here is honest all the same.
		return checkNamespaceAttempt{}, fmt.Errorf("generating the check's namespace identity: %w", err)
	}
	return checkNamespaceAttempt{
		name:   checkNamespacePrefix + "-" + hex.EncodeToString(b[:6]),
		marker: hex.EncodeToString(b[6:]),
	}, nil
}

// baseTarget returns one attempt's target before any child is written:
// the residue registration from the moment the namespace exists, with
// the marker everything this attempt stamps and children that have
// not been issued yet.
func (a checkNamespaceAttempt) baseTarget(peer verifiedCluster, cs kubernetes.Interface, nsUID types.UID) nodePortTarget {
	return nodePortTarget{
		peer:   peer,
		client: cs,
		nsName: a.name,
		nsUID:  nsUID,
		marker: a.marker,
		svc:    probeChild{kind: "service", name: nodePortService},
		slice:  probeChild{kind: "endpointslice", name: nodePortSlice},
	}
}

// resolveCheckNamespaceCreate settles one ambiguous namespace create:
// the create errored, but an error is not proof the server did not
// persist it — the response can be lost past the write (the setup
// deadline expiring mid-body, a connection reset), and client-go's
// own retry of a failed attempt can commit the first one and surface
// the refusal of the second as the error. One read of the name, under
// its own bounded context (never the setup's — the deadline that
// broke the create must not break the read that settles what it left
// behind) and through the same client the create went through,
// decides by the attempt's marker:
//
//   - a namespace carrying the marker is this attempt's: the target is
//     returned with the namespace fields set, so the caller's cleanup
//     removes it under the UID the read observed — deletion stays
//     preconditioned on ownership, never on the name alone;
//   - a namespace without the marker is not this attempt's (the name
//     was already held): it is left entirely alone, and the failure
//     names it;
//   - no namespace under the name, as of the read: the create did not
//     take effect. A create still in flight can commit after a read,
//     so the failure carries the name and the marker a late landing
//     would carry;
//   - a read that cannot say: the outcome is unresolved, and the
//     failure reports exactly that, with the name and the marker.
//
// This is deliberately one read, not the scc-admission probe's
// retrying resolver: the namespace is scaffolding, not the
// observation, so a failed preparation fails — nothing here adopts
// the namespace and continues.
func resolveCheckNamespaceCreate(cs kubernetes.Interface, peer verifiedCluster, attempt checkNamespaceAttempt, createErr error) (nodePortTarget, error) {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	ns, err := cs.CoreV1().Namespaces().Get(ctx, attempt.name, metav1.GetOptions{})
	switch {
	case err != nil && !apierrors.IsNotFound(err):
		return nodePortTarget{}, fmt.Errorf(
			"creating the check's namespace %s: %v; whether it was created could not be determined (reading it back: %v) — if it appears it carries this attempt's marker %q in label %s and is this check's to remove by hand",
			attempt.name, createErr, err, attempt.marker, checkAttemptLabelKey)
	case err == nil && ns.Labels[checkAttemptLabelKey] == attempt.marker:
		return attempt.baseTarget(peer, cs, ns.UID),
			fmt.Errorf("creating the check's namespace %s: %v; the create had in fact committed (the follow-up read found it carrying this attempt's marker), and the check's cleanup removes it",
				attempt.name, createErr)
	case err == nil:
		return nodePortTarget{}, fmt.Errorf(
			"creating the check's namespace %s: %v; the name is held by a namespace this check did not create (it carries no attempt marker), so it is left alone",
			attempt.name, createErr)
	default:
		return nodePortTarget{}, fmt.Errorf(
			"creating the check's namespace %s: %v; a follow-up read found no namespace of that name, so the create did not take effect (a create still in flight can commit after a read; one that appears carries this attempt's marker %q in label %s)",
			attempt.name, createErr, attempt.marker, checkAttemptLabelKey)
	}
}

// stillOurNamespace reads the probe namespace and confirms it is still
// the instance the create returned, immediately before a child (the
// Service, the EndpointSlice) is written into it. A namespace replaced
// under the same name while the check runs is not the check's to write
// into: the child would land in the replacement and survive the
// UID-preconditioned namespace delete. The check cannot close the
// window between this read and the write after it — that race is what
// the children's UIDs cover at cleanup — it refuses every replacement
// it can see.
func stillOurNamespace(ctx context.Context, cs kubernetes.Interface, name string, uid types.UID) error {
	ns, err := cs.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("the probe namespace %s is gone (it was deleted while the check ran); refusing to write the check's objects into whatever may replace it", name)
	case err != nil:
		return fmt.Errorf("re-reading the probe namespace %s: %w", name, err)
	case ns.UID != uid:
		return fmt.Errorf("the probe namespace %s was replaced while the check ran (a different instance now carries the name); refusing to write the check's objects into the replacement", name)
	}
	return nil
}

// apiserverEndpoint extracts the address and port of the apiserver's
// advertise endpoint from the default kubernetes service's
// EndpointSlices — the control-plane node's IP and the apiserver port,
// which the cluster itself routes its own kubernetes service to. The
// v1 Endpoints API carrying the same data is deprecated (v1.33+), so
// the slice is the read. Both fields the connect needs are optional in
// discovery/v1, so every candidate is validated: ports without values
// and addresses that do not parse are skipped and remembered, and an
// exhaustion of candidates is an error carrying every problem — never a
// zero-value endpoint handed to a connect.
func apiserverEndpoint(slices *discoveryv1.EndpointSliceList) (netip.Addr, int32, error) {
	var problems []string
	for _, slice := range slices.Items {
		var port int32
		havePort := false
		for _, p := range slice.Ports {
			if p.Port != nil {
				port, havePort = *p.Port, true
				break
			}
		}
		if !havePort {
			if len(slice.Ports) == 0 {
				problems = append(problems, fmt.Sprintf("slice %s carries no ports", slice.Name))
			} else {
				problems = append(problems, fmt.Sprintf("slice %s carries ports with no values", slice.Name))
			}
			continue
		}
		for _, endpoint := range slice.Endpoints {
			for _, address := range endpoint.Addresses {
				addr, err := netip.ParseAddr(address)
				if err != nil {
					problems = append(problems, fmt.Sprintf("address %q is not an IP", address))
					continue
				}
				return addr, port, nil
			}
		}
	}
	if len(problems) == 0 {
		problems = append(problems, "no endpoint carries an address")
	}
	return netip.Addr{}, 0, fmt.Errorf("the kubernetes service's EndpointSlices carry no usable apiserver endpoint: %s", strings.Join(problems, "; "))
}

// corroborateAPIEndpoint proves the endpoint read from a cluster's own
// API — the whole endpoint, address and port together — is one of the
// control-plane API endpoints the provisioner itself reports for that
// cluster, and returns the matched endpoint: the value the check binds
// its Service backend to. What the cluster's API reports is writable by
// whoever can edit the reporting objects, so the endpoint alone does
// not identify what a connect would test: a stale or tampered endpoint
// could name an unrelated listener — well-formed, routable, and not
// this cluster's apiserver — or another node entirely, and the check
// would report a PASS that tested nothing. The provisioner's report is
// the other half: the runtime's own knowledge of where the node lives
// on the network the clusters share and what port it configured the
// apiserver to listen on there, which no in-cluster edit rewrites. The
// match is on the full endpoint, so a corroborated address with a
// different port — the shape of an endpoint pointing at another
// listener on the right node — is as much a refusal as a foreign
// address. The cluster-reported port is validated before it is
// narrowed to the endpoint type (netip.AddrPort carries a uint16):
// an int32 outside 1-65535 must be refused, never truncated into a
// value that could match — the provisioner's port plus 2^16 is exactly
// such a crafted value. The whole resolution is bounded by ctx.
func corroborateAPIEndpoint(ctx context.Context, resolver provider.ControlPlaneAPIEndpointResolver, cluster string, addr netip.Addr, port int32) (netip.AddrPort, error) {
	if port < 1 || port > 65535 {
		return netip.AddrPort{}, fmt.Errorf(
			"the apiserver endpoint port %d, read from %s's own API, is outside the valid range 1-65535", port, cluster)
	}
	endpoints, err := resolver.ControlPlaneAPIEndpoints(ctx, cluster)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("reading the control-plane API endpoints of %s from the provisioner: %w", cluster, err)
	}
	reported := netip.AddrPortFrom(addr, uint16(port))
	for _, endpoint := range endpoints {
		if endpoint == reported {
			return endpoint, nil
		}
	}
	return netip.AddrPort{}, fmt.Errorf(
		"the apiserver endpoint %s, read from %s's own API, is not one of the control-plane API endpoints the provisioner reports for it (%s) — "+
			"the endpoint may be stale or tampered with, and the check refuses to point its Service and its connect at an endpoint it cannot prove is this cluster's apiserver",
		reported, cluster, joinEndpointText(endpoints))
}

// joinEndpointText renders an endpoint set for an error message.
func joinEndpointText(endpoints []netip.AddrPort) string {
	parts := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		parts = append(parts, endpoint.String())
	}
	return strings.Join(parts, ", ")
}

// cleanNodePortTarget removes the probe's residue from one target and
// confirms it gone. Deletion of the namespace requires its UID — the
// ownership proof from the create (or from the resolving read) — as a
// precondition: a namespace replaced after creation is not this
// check's to remove, and the API refuses the delete instead of taking
// the replacement.
//
// No deletion's answer is taken as proof of absence. A 409 on the
// namespace delete is not only the replacement case — the API answers
// an admission refusal the same way, with the original still live —
// and an accepted delete returns while the namespace controller
// still drains the contents. The proof is the read behind
// kube.WaitForUIDGone, under this cleanup's own budget: NotFound or
// a different UID proves the instance the check created is gone.
// Only when a different instance holds the name — the replacement
// its racing children landed in, children the namespace delete
// would have cascaded — are the children removed, each by its own
// UID, under a second deadline that starts only once the name is
// known held: confirming that can spend most of the first deadline
// (a read that answers late, a finalizer holding the original past
// its accepted delete), and children run under the remainder would
// have their deletes canceled mid-operation. The cleanup runs under
// its own bounded contexts, never the check's: a cancelled or
// expired check still removes what it created.
func (c *Client) cleanNodePortTarget(target nodePortTarget) error {
	ctx, cancel := context.WithTimeout(context.Background(), nsDeleteTimeout)
	defer cancel()
	err := target.client.CoreV1().Namespaces().Delete(ctx, target.nsName, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &target.nsUID},
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("deleting the %s namespace: %w", target.nsName, err)
	}
	held, err := kube.WaitForUIDGone(ctx,
		func(ctx context.Context) (metav1.Object, error) {
			return target.client.CoreV1().Namespaces().Get(ctx, target.nsName, metav1.GetOptions{})
		}, target.nsUID, nsDeleteTimeout)
	if err != nil {
		return fmt.Errorf("confirming the %s namespace deleted: %w", target.nsName, err)
	}
	if !held {
		// The name is free: nothing of the check's can remain under
		// it — the children were inside the namespace, and it is
		// gone.
		return nil
	}
	// A fresh deadline for the children, started only now that the
	// replacement is confirmed: the namespace phase above may have
	// spent most of its own budget reaching that answer, and children
	// run under the remainder would have their deletes canceled
	// mid-operation — the transport refusing them locally before the
	// server finished, the residue left behind.
	childCtx, childCancel := context.WithTimeout(context.Background(), nsDeleteTimeout)
	defer childCancel()
	return cleanProbeChildren(childCtx, target)
}

// cleanProbeChildren removes the Service and EndpointSlice the check
// created (or tried to create) inside a namespace it can no longer
// delete — the name is held by an instance the check did not create.
// It runs under the child phase's own fresh deadline, which
// cleanNodePortTarget starts only after confirming the replacement,
// so the namespace phase's spending cannot starve the children.
// Within that deadline the children are cleaned at the same time,
// each from its start: a child's confirmation can poll to the
// deadline's end (a finalizer holding the object past its accepted
// delete), and a child cleaned second would find the deadline spent —
// its delete refused locally by the transport, never reaching the
// API. Started together, every delete is issued while the deadline
// still holds, one unremovable child still does not spare the other,
// and both errors are reported.
func cleanProbeChildren(ctx context.Context, target nodePortTarget) error {
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := func(i int, child probeChild,
		read func(context.Context) (metav1.Object, error),
		del func(ctx context.Context, name string, opts metav1.DeleteOptions) error,
	) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := cleanProbeChild(ctx, target, child, read, del); err != nil {
				errs[i] = fmt.Errorf("removing the check's %s: %w", child.kind, err)
			}
		}()
	}
	start(0, target.svc,
		func(ctx context.Context) (metav1.Object, error) {
			return target.client.CoreV1().Services(target.nsName).Get(ctx, target.svc.name, metav1.GetOptions{})
		},
		target.client.CoreV1().Services(target.nsName).Delete)
	start(1, target.slice,
		func(ctx context.Context) (metav1.Object, error) {
			return target.client.DiscoveryV1().EndpointSlices(target.nsName).Get(ctx, target.slice.name, metav1.GetOptions{})
		},
		target.client.DiscoveryV1().EndpointSlices(target.nsName).Delete)
	wg.Wait()
	return errors.Join(errs...)
}

// cleanProbeChild removes one child the check wrote (or tried to
// write) into a namespace it can no longer delete. A child whose
// create answered carries its UID, and the delete is preconditioned
// on it: the API refuses the delete onto anything that is not the
// check's child. A child whose create was issued but lost its
// response — the same ambiguity the namespace create's resolution
// settles — carries no UID, so it is resolved by the attempt marker
// on one bounded read of its name: a marker-matched child is deleted
// under the UID the read observed, a child without the marker is not
// this check's and is left alone, a free name means the create did
// not take effect, and an unreadable one is reported with the
// namespace, the child, and the marker a late landing would carry.
// Every removal is then confirmed by read — a delete's return is a
// request, not a result — under the child phase's budget: NotFound or
// a different UID proves the child gone.
func cleanProbeChild(ctx context.Context, target nodePortTarget, child probeChild,
	read func(context.Context) (metav1.Object, error),
	del func(ctx context.Context, name string, opts metav1.DeleteOptions) error,
) error {
	if !child.issued {
		// The create was never sent: nothing of the check's can exist
		// under the child's name.
		return nil
	}
	uid := child.uid
	if uid == "" {
		obj, err := read(ctx)
		switch {
		case apierrors.IsNotFound(err):
			// The create did not take effect (a create still in
			// flight can commit after a read; one that appears
			// carries the marker).
			return nil
		case err != nil:
			return fmt.Errorf(
				"whether the %s %s/%s was created could not be determined (reading it back: %v) — if it appears it carries this attempt's marker %q in label %s and is this check's to remove by hand",
				child.kind, target.nsName, child.name, err, target.marker, checkAttemptLabelKey)
		case obj.GetLabels()[checkAttemptLabelKey] != target.marker:
			// The name is held by an object this check did not
			// create: not this check's residue, left alone.
			return nil
		default:
			uid = obj.GetUID()
		}
	}
	if err := deletePreconditioned(ctx, child.name, uid, del); err != nil {
		return fmt.Errorf("deleting the %s %s/%s: %w", child.kind, target.nsName, child.name, err)
	}
	if _, err := kube.WaitForUIDGone(ctx, read, uid, nsDeleteTimeout); err != nil {
		return fmt.Errorf("confirming the %s %s/%s deleted: %w", child.kind, target.nsName, child.name, err)
	}
	return nil
}

// deletePreconditioned deletes one named object under a UID
// precondition, treating NotFound and Conflict as outcomes the
// caller's follow-up confirmation settles: the object is either
// already gone or not this check's. The confirmation is the read
// that follows (kube.WaitForUIDGone), never the delete's return.
func deletePreconditioned(ctx context.Context, name string, uid types.UID,
	delete func(ctx context.Context, name string, opts metav1.DeleteOptions) error,
) error {
	err := delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return err
	}
	return nil
}

// reachNodePort connects from the source cluster's control-plane node
// to one target's NodePort through the provisioner's TCP-probe
// capability. The whole attempt is bounded by connectTimeout; the
// probe itself retries inside the node for about twenty seconds
// (kube-proxy needs a beat to program a freshly created EndpointSlice).
func (c *Client) reachNodePort(ctx context.Context, prober provider.TCPProber, source string, target nodePortTarget) error {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := prober.ProbeTCPFromNode(ctx, source, target.nodeAddr, int(target.nodePort)); err != nil {
		return fmt.Errorf("connecting to node %s:%d from %s: %w", target.nodeAddr, target.nodePort, source, err)
	}
	c.log.Info("cross-cluster NodePort reachable",
		"from", source, "to", target.peer.name, "node", target.nodeAddr.String(), "nodePort", target.nodePort)
	return nil
}

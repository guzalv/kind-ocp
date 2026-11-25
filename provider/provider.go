// Package provider defines the cluster-provisioning boundary. kind-ocp
// currently ships one implementation (kind, as a library); the interface
// exists so future runtimes can plug in without touching the provisioning
// loop.
package provider

import (
	"context"
	"net/netip"
)

// Provisioner creates and manages the clusters that kind-ocp layers are
// applied to.
//
// One instance serves a whole create run, whose clusters 2..N are
// provisioned concurrently: every method may be called concurrently with
// itself for different clusters, and implementations must guard their own
// state accordingly (kind-ocp keeps per-cluster state on its side of the
// boundary).
type Provisioner interface {
	// Create creates the cluster named in opts if it does not already exist
	// and reports whether it was newly created. An existing cluster is
	// reused (reconcile semantics) and its kubeconfig re-exported to
	// opts.KubeconfigPath, which covers a state directory deleted out from
	// under a live cluster. Create returns once the cluster is provisioned;
	// node readiness is the caller's job through the exported kubeconfig
	// (kind's own wait reports a timeout as success, so it is not used).
	// A failed create keeps the cluster in place for debugging when the
	// implementation supports it (kind: retain).
	//
	// Create may be called concurrently for different clusters (see the
	// interface comment).
	Create(ctx context.Context, opts CreateOptions) (created bool, err error)

	// Delete removes the cluster. Deleting a cluster that does not exist is
	// not an error (idempotent). kubeconfigPath, when non-empty, is the file
	// the cluster's context is recorded in and is cleaned up too.
	Delete(ctx context.Context, name string, kubeconfigPath string) error

	// List returns the names of all clusters known to the provisioner.
	List(ctx context.Context) ([]string, error)

	// ExportKubeconfig writes the cluster's kubeconfig to path.
	ExportKubeconfig(ctx context.Context, name string, path string) error
}

// ImageLoader is an optional Provisioner capability: a runtime whose
// clusters can import images without a registry implements it. The kind
// provisioner does. The framework binds its layers.Env image-loading
// service only when the provisioner implements this interface — a
// lifecycle-only provider leaves the service unset, and a layer that
// needs it (scc-admission builds a local image) then refuses to run
// instead of the provider being forced to stub the method.
type ImageLoader interface {
	// LoadImage loads an image that exists on the local Docker daemon
	// into the named cluster's container runtime, so workloads on the
	// cluster can run it without a registry. The image must already be
	// present locally; the provisioner never builds or pulls. Loading
	// into every node of the cluster is idempotent: an image already
	// carrying the exact reference is left alone.
	LoadImage(ctx context.Context, cluster string, imageRef string) error
}

// ImageBuilder is an optional Provisioner capability: a runtime with a
// local container-image builder implements it, so a layer can build the
// image it runs from sources this repo embeds. The kind provisioner
// implements it through the Docker daemon it already drives. The
// framework binds its layers.Env image-building service only when the
// provisioner implements this interface — a runtime that cannot build
// images locally leaves the service unset, and a layer that needs it
// (scc-admission builds a local image) then refuses to run instead of
// the provider being forced to stub the method.
//
// The contract is deliberately runtime-neutral: the caller hands over a
// populated build-context directory and the reference to tag; how the
// runtime turns one into the other is the implementation's business.
// No layer may invoke a container-runtime CLI directly — a podman
// implementation of this capability must be a drop-in replacement.
type ImageBuilder interface {
	// BuildImage builds the image tagged ref from the files in
	// contextDir and leaves it on the local daemon the runtime's other
	// capabilities (ImageLoader) import from, so the caller can load
	// the built image into its clusters without a registry. The
	// directory's contents are the caller's; the implementation reads
	// them but never modifies them. The whole build is bounded by ctx.
	BuildImage(ctx context.Context, ref, contextDir string) error
}

// HostLimitRaiser is an optional Provisioner capability: a runtime whose
// clusters are containers on this host implements it so create can raise
// the host-level limits several concurrent clusters need. The kind
// provisioner implements it for fs.inotify.max_user_instances — every
// node's containerd, kubelet, and apiserver hold inotify watches, and the
// default limit (often 128) runs short with several clusters.
//
// RaiseHostLimits is best-effort by contract: implementations log
// refusals and never fail the run — a host kind-ocp cannot tune must
// still get its clusters. Create calls it once, before the first cluster
// is provisioned. A runtime without the capability (a remote runtime, a
// test double) is simply not asked, so nothing but a real container
// runtime on this host ever changes host settings.
type HostLimitRaiser interface {
	RaiseHostLimits(ctx context.Context)
}

// TCPProber is an optional Provisioner capability: a runtime that can
// reach its cluster nodes directly implements it, so the cross-cluster
// NodePort check can connect from a source cluster's node — the network
// namespace every pod's off-cluster traffic is masqueraded through —
// without pulling an image into the cluster. A provider without the
// capability leaves that check skipped with that reason, never failed:
// the runtime simply cannot answer the question.
type TCPProber interface {
	// ProbeTCPFromNode opens one TCP connection from the named cluster's
	// control-plane node to addr:port, retrying briefly (kube-proxy needs
	// a beat to program a freshly created NodePort), and returns an
	// error naming the endpoint when the connection cannot be
	// established. The whole attempt is bounded by ctx. How the connect
	// runs is the implementation's business; the caller supplies only
	// the validated typed endpoint.
	ProbeTCPFromNode(ctx context.Context, cluster string, addr netip.Addr, port int) error
}

// ControlPlaneAPIEndpointResolver is an optional Provisioner
// capability: a runtime that knows where its clusters' apiservers
// listen — the control-plane node's address and port on the network
// its clusters share — implements it. The cross-cluster NodePort check
// corroborates the endpoint it reads from a cluster's own API against
// this, as one endpoint, and binds its Service backend to the match:
// the objects reporting that endpoint inside the cluster are writable
// by whoever can edit them, so the endpoint alone would let whoever
// edits it choose both the node and the listener the check publishes a
// NodePort to — and a TCP connect proves that a listener answered,
// never which one, nor on which node. A provider without the
// capability leaves the check skipped with that reason, never run
// with a backend it cannot attribute.
type ControlPlaneAPIEndpointResolver interface {
	// ControlPlaneAPIEndpoints returns the endpoints of the named
	// cluster's apiserver as the runtime itself knows them — for kind,
	// the control-plane node's addresses on the shared network, each
	// with the port kind configured that apiserver to listen on inside
	// the node. The caller treats the set as the apiserver's identity:
	// an endpoint read from the cluster's API that is not one of these
	// is not provably this cluster's apiserver. The whole resolution is
	// bounded by ctx.
	ControlPlaneAPIEndpoints(ctx context.Context, cluster string) ([]netip.AddrPort, error)
}

// CreateOptions describes one cluster creation.
type CreateOptions struct {
	// Name is the cluster name (also the node container name prefix).
	Name string
	// NodeImage is the kind node image; the default is digest-pinned.
	NodeImage string
	// KubeconfigPath is where the kubeconfig is written. Its parent
	// directory must exist.
	KubeconfigPath string
}

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stackrox/kind-ocp/config"
	"github.com/stackrox/kind-ocp/internal/fscheck"
	"github.com/stackrox/kind-ocp/kindocp"
)

// resolveConfig resolves the example's flags through kind-ocp's own
// resolution path, so defaults and validation are exactly the product's.
// The example never reads a config file or KIND_OCP_* environment: it is a
// recipe with its own shape (always profile full), and surprising it with an
// ambient config would change what it deploys.
func resolveConfig(prefix, stateRoot *string, count *int) (*config.Config, error) {
	flags := config.Flags{}
	if *prefix != "" {
		flags.Prefix = prefix
	}
	if *count != 0 {
		flags.Count = count
	}
	if *stateRoot != "" {
		flags.StateRoot = stateRoot
	}
	return config.Resolve(flags, config.Env{})
}

// ensureClusters creates or reuses the prefix's clusters through the
// kind-ocp façade and returns the records plus each cluster's kubeconfig
// path. Reuse is the façade's own reconcile: a matching, instance-bound
// record is converged; anything else is rejected with recovery
// instructions. The example adds no policy of its own.
func ensureClusters(ctx context.Context, cfg *config.Config, log *slog.Logger) ([]kindocp.Record, map[string]string, error) {
	client, err := kindocp.New(kindocp.Options{Config: cfg, Log: log})
	if err != nil {
		return nil, nil, fmt.Errorf("building the kind-ocp client: %w", err)
	}
	records, err := client.Create(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("creating the clusters (they are kept for debugging; kind-ocp delete cleans up): %w", err)
	}
	kubeconfigs := make(map[string]string, len(records))
	for _, rec := range records {
		path, err := client.Kubeconfig(ctx, rec.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving the kubeconfig for %s: %w", rec.Name, err)
		}
		kubeconfigs[rec.Name] = path
	}
	return records, kubeconfigs, nil
}

// openWorkDir prepares the example's private work directory, where the
// chart archives are extracted and the secret values files are written.
// It sits beside kind-ocp's state root — the same caller-owned
// neighborhood — and deliberately neither inside the state root (the
// store's verified tree holds kind-ocp's own state, not foreign files;
// `kind-ocp delete` removes that tree, credentials included, so the two
// must never overlap) nor under the system temporary directory (a
// predictable path there has parents another local user can substitute,
// and tmp reaping would silently invalidate the reused admin password).
//
// The location is derived from the resolved state base, not the
// configured spelling: the state root is resolved through kind-ocp's own
// walk (fscheck.ResolveDir — it exists by now; the clusters were created
// first), and the workspace is its sibling named after the state root
// itself. Deriving from the resolved base makes the workspace a distinct
// sibling by construction: no state root spelling — not even one ending
// in the workspace's own name — can make the two trees coincide, and
// aliases in the configured spelling cannot move one inside the other.
// What an alias of the workspace itself could still do is resolved and
// refused: before anything is created, an existing workspace base is
// resolved and held disjoint from the state root, and the final resolved
// workspace is checked against the same rule again — the invariant holds
// for the exact path the run writes through.
//
// The directory is opened through kind-ocp's own filesystem boundary
// (internal/fscheck — the same policy the state tree walks; the example
// ships in the same module, so it reuses the implementation instead of
// forking it): every ancestor is verified before it is trusted,
// substitutable ancestors and symlinks are refused, and the directory
// itself is created 0700 or verified caller-owned and private. The
// resolved path it returns is the one the run writes through — never
// the alias it started from.
func openWorkDir(cfg *config.Config) (string, error) {
	stateBase, err := fscheck.ResolveDir(cfg.StateRoot)
	if err != nil {
		return "", fmt.Errorf("resolving the kind-ocp state root %s: %w", cfg.StateRoot, err)
	}
	workBase := filepath.Join(filepath.Dir(stateBase), filepath.Base(stateBase)+workDirSuffix)

	// An existing workspace base may be an alias — including one into
	// the state tree. Resolve and refuse that before anything is
	// created below it; a missing base is the one shape that cannot
	// hide an alias (nothing is there to point anywhere), and it is
	// left to the creating walk below.
	if resolved, err := fscheck.ResolveDir(workBase); err == nil {
		if err := refuseTreeOverlap(resolved, stateBase); err != nil {
			return "", err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("resolving the work directory base %s: %w", workBase, err)
	}

	dir := filepath.Join(workBase, cfg.Prefix)
	resolved, err := fscheck.OpenManagedDir(dir)
	if err != nil {
		return "", fmt.Errorf("preparing the work directory %s: %w", dir, err)
	}
	if err := refuseTreeOverlap(resolved, stateBase); err != nil {
		return "", err
	}
	return resolved, nil
}

// refuseTreeOverlap reports an error when the credential workspace and
// the kind-ocp state tree are not disjoint. Both paths arrive fully
// resolved, so the rule holds for the trees as the run will actually
// write them. A workspace inside the state root would have its
// credentials removed by `kind-ocp delete`; a state root inside the
// workspace would sweep kind-ocp's state through the credential
// directory. Either way the run stops before any credential exists.
func refuseTreeOverlap(work, stateBase string) error {
	if work == stateBase || strings.HasPrefix(work, stateBase+string(filepath.Separator)) {
		return fmt.Errorf(
			"the work directory %s resolves inside the kind-ocp state root %s — the workspace must stay outside it (kind-ocp delete removes the state tree, credentials included); point the state root or the workspace elsewhere",
			work, stateBase)
	}
	if strings.HasPrefix(stateBase, work+string(filepath.Separator)) {
		return fmt.Errorf(
			"the kind-ocp state root %s resolves inside the work directory %s — the two trees must stay disjoint; point the state root or the workspace elsewhere",
			stateBase, work)
	}
	return nil
}

// workDirSuffix names the workspace beside the state root: the resolved
// state base's own name with this suffix, holding one per-prefix
// directory. The default state root (…/kind-ocp) therefore keeps the
// workspace at …/kind-ocp-stackrox-example, and no state root can
// derive a workspace that coincides with it.
const workDirSuffix = "-stackrox-example"

// restConfigFor loads a kubeconfig the way an outside consumer does: the
// explicit path, no ambient context merging. kind-ocp's state kubeconfigs
// are self-contained.
func restConfigFor(kubeconfigPath string) (*rest.Config, error) {
	loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %s: %w", kubeconfigPath, err)
	}
	return restConfig, nil
}

func clientsetFor(kubeconfigPath string) (kubernetes.Interface, *rest.Config, error) {
	restConfig, err := restConfigFor(kubeconfigPath)
	if err != nil {
		return nil, nil, err
	}
	cs, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("building a client for %s: %w", kubeconfigPath, err)
	}
	return cs, restConfig, nil
}

// The names of the objects the chart installs that the example reads back.
// They are chart constants (the release name and namespace are fixed by the
// chart's own defaults), not choices of this example.
const (
	stackroxNamespace   = "stackrox"
	centralServiceName  = "central"
	centralExposureName = "central-loadbalancer"
	centralPodLabel     = "app=central"
	centralPodPort      = 8443

	// centralTLSSecretName is the secret the chart's pre-install hook
	// writes: Central's serving certificate, its private key, and the
	// CA that signed them (ca.pem). The example reads the CA through
	// the authenticated Kubernetes API and verifies the port-forwarded
	// Central with it — the trust the cluster's own admin already
	// holds, not a leap of faith in whatever answers on the local port.
	centralTLSSecretName = "central-tls"

	// centralTLSCAKey is the CA entry inside the central-tls secret.
	centralTLSCAKey = "ca.pem"

	// centralTLSServerName is the DNS name the chart's generated
	// serving certificate carries (the chart's computeSANs always
	// includes the fixed `stackrox` namespace and the `central` service
	// base name). The client dials the forwarded loopback address but
	// verifies the certificate against this name, so a certificate
	// without Central's identity does not pass even when the TCP
	// endpoint is the right one.
	centralTLSServerName = "central.stackrox.svc"
)

// centralCAPEM returns the CA that roots Central's serving certificate.
// It is read through the authenticated Kubernetes API — the same
// kubeconfig the port-forward itself uses — from the secret the chart's
// install hook wrote, so the trust in Central's identity is anchored
// where the cluster's own authentication already is, not in the tunnel.
func centralCAPEM(ctx context.Context, cs kubernetes.Interface) ([]byte, error) {
	secret, err := cs.CoreV1().Secrets(stackroxNamespace).Get(ctx, centralTLSSecretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading secret %s/%s (the chart's install hook writes it; was Central deployed with the central-services chart?): %w",
			stackroxNamespace, centralTLSSecretName, err)
	}
	caPEM := secret.Data[centralTLSCAKey]
	if len(caPEM) == 0 {
		return nil, fmt.Errorf("secret %s/%s carries no %q entry", stackroxNamespace, centralTLSSecretName, centralTLSCAKey)
	}
	return caPEM, nil
}

// centralEndpoint resolves the address Sensors use to reach Central: the
// central cluster's control-plane node address on the shared kind network
// (every cluster in the prefix attaches to it, which is what makes this
// cross-cluster path work) plus the NodePort the chart's exposure service
// allocated.
func centralEndpoint(ctx context.Context, cs kubernetes.Interface) (string, error) {
	addr, err := controlPlaneNodeAddress(ctx, cs)
	if err != nil {
		return "", err
	}
	nodePort, err := centralNodePort(ctx, cs)
	if err != nil {
		return "", err
	}
	if addr.Is4() {
		return fmt.Sprintf("https://%s:%d", addr, nodePort), nil
	}
	return fmt.Sprintf("https://[%s]:%d", addr, nodePort), nil
}

// controlPlaneNodeAddress returns the control-plane node's internal
// address — the node container's address on the kind network. kind
// clusters built by kind-ocp are single-node, so exactly one control-plane
// node is expected; more than one is not an error (any node's address
// serves the NodePort), but none with an address is. IPv4 is preferred —
// the shared network is dual-stack and both families work, but the
// composed endpoint URL then needs no bracket formatting.
func controlPlaneNodeAddress(ctx context.Context, cs kubernetes.Interface) (netip.Addr, error) {
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/control-plane"})
	if err != nil {
		return netip.Addr{}, fmt.Errorf("listing control-plane nodes: %w", err)
	}
	var v6 netip.Addr
	for _, node := range nodes.Items {
		for _, addr := range node.Status.Addresses {
			if addr.Type != corev1.NodeInternalIP || addr.Address == "" {
				continue
			}
			parsed, err := netip.ParseAddr(addr.Address)
			if err != nil {
				continue
			}
			if parsed.Is4() {
				return parsed, nil
			}
			v6 = parsed
		}
	}
	if v6.IsValid() {
		return v6, nil
	}
	return netip.Addr{}, errors.New("no control-plane node reports an internal address")
}

// centralNodePort reads the NodePort the chart's exposure service allocated
// for its HTTPS port. The example does not pin a port: Kubernetes picks
// one, and reading it back is what makes the recipe re-runnable.
func centralNodePort(ctx context.Context, cs kubernetes.Interface) (int32, error) {
	svc, err := cs.CoreV1().Services(stackroxNamespace).Get(ctx, centralExposureName, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("reading service %s/%s (was Central deployed with central.exposure.nodePort.enabled?): %w",
			stackroxNamespace, centralExposureName, err)
	}
	for _, port := range svc.Spec.Ports {
		if port.Port == 443 {
			if port.NodePort == 0 {
				return 0, fmt.Errorf("service %s/%s exposes port 443 without a NodePort", stackroxNamespace, centralExposureName)
			}
			return port.NodePort, nil
		}
	}
	return 0, fmt.Errorf("service %s/%s has no port 443", stackroxNamespace, centralExposureName)
}

// centralPodName finds the running Central pod for the port-forward. The
// port-forward targets a pod, not the service; Central's container listens
// on centralPodPort behind the service's 443.
func centralPodName(ctx context.Context, cs kubernetes.Interface) (string, error) {
	pods, err := cs.CoreV1().Pods(stackroxNamespace).List(ctx, metav1.ListOptions{LabelSelector: centralPodLabel})
	if err != nil {
		return "", fmt.Errorf("listing Central pods: %w", err)
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning {
			return pod.Name, nil
		}
	}
	return "", errors.New("no running Central pod found")
}

// apiServerURL normalizes a kubeconfig's server URL for building
// subresource paths (the port-forward dialer needs the full URL).
func apiServerURL(restConfig *rest.Config) (*url.URL, error) {
	u, err := url.Parse(restConfig.Host)
	if err != nil {
		return nil, fmt.Errorf("parsing the API server URL %q: %w", restConfig.Host, err)
	}
	return u, nil
}

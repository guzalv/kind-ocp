package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"

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
// The two trees cannot overlap, and no comparison enforces that — the
// construction does. The state root is resolved first (fscheck.ResolveDir
// — it exists by now; the clusters were created first), and the
// workspace is then *derived* from that resolved base: the resolved
// state base's own sibling, named after the state root itself, holding
// one per-prefix directory. The derivation is entered through the same
// walk the state store uses (fscheck.WalkDirNames) with the resolved
// base's parent as the anchor and alias following disabled: the anchor's
// components keep the ancestor policy, the two derived components get
// the managed policy, and a symlink at the workspace base or leaf is
// refused outright, wherever it points. That closes the
// case-insensitive hole a spelling check cannot — on macOS volumes,
// `State` and `state` are one directory while remaining two strings, so
// an alias planted at the workspace base naming the state root in the
// other case passed every lexical overlap check while pointing the
// credentials straight into the tree `kind-ocp delete` removes. The
// refusal happens before anything is created; the workspace the run
// writes through is exactly the sibling the derivation named.
func openWorkDir(cfg *config.Config) (string, error) {
	stateBase, err := fscheck.ResolveDir(cfg.StateRoot)
	if err != nil {
		return "", fmt.Errorf("resolving the kind-ocp state root %s: %w", cfg.StateRoot, err)
	}
	anchorNames := fscheck.PathNames(filepath.Dir(stateBase))
	names := append(anchorNames, workDirBaseName(stateBase), cfg.Prefix)
	resolved, err := fscheck.WalkDirNames(names, len(anchorNames), fscheck.WalkOptions{Create: true})
	if err != nil {
		return "", fmt.Errorf("preparing the work directory beside the state root %s: %w", stateBase, err)
	}
	return resolved, nil
}

// workDirSuffix names the workspace beside the state root: the resolved
// state base's own name with this suffix, holding one per-prefix
// directory. The default state root (…/kind-ocp) therefore keeps the
// workspace at …/kind-ocp-stackrox-example, and no state root can
// derive a workspace that coincides with it.
const workDirSuffix = "-stackrox-example"

// workDirBaseName names the work directory's base for a state base: the
// base's own sibling, named after the state base itself with
// workDirSuffix. This is the one derivation of the workspace's name —
// openWorkDir's creating walk and the startup override check
// (workspaceBase) both name the workspace through it, so the tree the
// check refuses and the tree the run rewrites are the same tree.
func workDirBaseName(stateBase string) string {
	return filepath.Base(stateBase) + workDirSuffix
}

// stateBaseCandidate names the state base for a run without requiring
// or creating anything: the resolved state base when it exists — the
// same resolution openWorkDir walks below — and its absolute spelling
// when it does not (the tree a fresh run is about to create beside).
// Callers compare by directory identity (os.Stat + os.SameFile), which
// follows alias spellings on both sides, so a lexical divergence
// between the candidate and the real tree cannot make one directory
// pass for two.
func stateBaseCandidate(stateRoot string) string {
	if base, err := fscheck.ResolveDir(stateRoot); err == nil {
		return base
	}
	base, _ := filepath.Abs(stateRoot)
	return base
}

// workspaceBase names the work directory's base for a state root — the
// tree the run rewrites (every per-prefix work directory lives under
// it) — without requiring or creating anything. The startup override
// check compares against it by directory identity, through the same
// workDirBaseName openWorkDir creates below.
func workspaceBase(stateRoot string) string {
	base := stateBaseCandidate(stateRoot)
	return filepath.Join(filepath.Dir(base), workDirBaseName(base))
}

// stateTreePath names this run's per-prefix kind-ocp state directory —
// <state root>/<prefix>, the tree the façade opens
// (kindocp.New builds its store from exactly that join) and the run
// rewrites: the expected-cluster count, the cluster records, the
// exported kubeconfigs, and the layer caches all live there, and
// `kind-ocp delete` removes the whole tree — without requiring or
// creating anything. The startup override check compares against it by
// directory identity, exactly as it does against the workspace.
func stateTreePath(stateRoot, prefix string) string {
	return filepath.Join(stateBaseCandidate(stateRoot), prefix)
}

// refuseValuesOverrideInTree refuses a values override inside one tree
// the run rewrites (what names the tree for the refusal). Containment
// is by directory identity, never by a string prefix: on a
// case-insensitive volume (macOS, a supported platform) two spellings
// that differ by case are one directory while remaining two strings —
// exactly the shape a prefix comparison would pass. Each directory is
// compared through os.Stat (which follows aliases) and os.SameFile, so
// the comparison is of the directories themselves. A tree that does
// not exist matches nothing, which is correct, because no file can be
// inside it.
func refuseValuesOverrideInTree(path, tree, what string) error {
	treeInfo, err := os.Stat(tree)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("checking %s %s: %w", what, tree, err)
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if fi, err := os.Stat(dir); err == nil && os.SameFile(fi, treeInfo) {
			return fmt.Errorf(
				"the values file %s is inside %s %s, which the run rewrites — the run would overwrite it; keep values files outside it",
				path, what, tree)
		}
		if filepath.Dir(dir) == dir {
			return nil
		}
	}
}

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

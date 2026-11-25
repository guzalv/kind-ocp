package main

import (
	"context"
	"errors"
	"fmt"
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
// It is deliberately outside kind-ocp's state root: the state root is a
// verified boundary the store walks, and foreign files have no business in
// it. The directory must be private to the invoking user; an existing
// group- or world-accessible directory is refused rather than reused.
func openWorkDir(prefix string) (string, error) {
	dir := filepath.Join(os.TempDir(), "kind-ocp-stackrox-example", prefix)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating the work directory %s: %w", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return "", fmt.Errorf("the work directory %s is too permissive (%o); it holds the Central admin password — remove it or chmod 700", dir, mode)
	}
	return dir, nil
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
)

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

// Package kind implements the provider.Provisioner interface with kind as a
// library (sigs.k8s.io/kind, pinned in go.mod). This is the only package
// that knows about kind; everything else sees provider.Provisioner.
package kind

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/constants"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
	"sigs.k8s.io/kind/pkg/log"

	"github.com/stackrox/kind-ocp/internal/fscheck"
	"github.com/stackrox/kind-ocp/provider"
)

// Provisioner provisions clusters with kind.
type Provisioner struct {
	prov *cluster.Provider
	log  *slog.Logger

	// docker runs the Docker CLI for the probe path. New installs the
	// real runner; tests inject a recorder to assert the exact argv
	// without a daemon.
	docker dockerRunner
}

// New returns a kind-based provisioner. It fails fast with a clear error
// when Docker (daemon and CLI) is unavailable: kind drives containers
// through the docker CLI. log receives kind's own progress output.
func New(log *slog.Logger) (*Provisioner, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := dockerAvailable(); err != nil {
		return nil, fmt.Errorf("kind-ocp requires Docker: %w", err)
	}
	return &Provisioner{
		prov: cluster.NewProvider(
			cluster.ProviderWithDocker(),
			cluster.ProviderWithLogger(kindLogger{log: log}),
		),
		log:    log,
		docker: runDocker,
	}, nil
}

// Create implements provider.Provisioner.
func (p *Provisioner) Create(ctx context.Context, opts provider.CreateOptions) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	exists, err := p.exists(opts.Name)
	if err != nil {
		return false, err
	}
	if exists {
		// Reconcile: keep the cluster, but make sure the kubeconfig is in
		// the state dir (it may have been lost with a deleted state root).
		if err := p.ExportKubeconfig(ctx, opts.Name, opts.KubeconfigPath); err != nil {
			return false, err
		}
		return false, nil
	}

	if err := p.prov.Create(opts.Name,
		cluster.CreateWithV1Alpha4Config(clusterConfig(opts)),
		// Retain failed nodes: kind's default is to delete the cluster when
		// any create step errors, which would silently discard the "keep
		// clusters for debugging" contract (AC-1.7).
		cluster.CreateWithRetain(true),
		cluster.CreateWithKubeconfigPath(opts.KubeconfigPath),
	); err != nil {
		return false, fmt.Errorf("creating kind cluster: %w", err)
	}
	if err := verifyKubeconfig(opts.KubeconfigPath); err != nil {
		return false, err
	}
	return true, nil
}

// Delete implements provider.Provisioner. Deleting a missing cluster is a
// no-op.
func (p *Provisioner) Delete(ctx context.Context, name string, kubeconfigPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exists, err := p.exists(name)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := p.prov.Delete(name, kubeconfigPath); err != nil {
		return fmt.Errorf("deleting kind cluster %s: %w", name, err)
	}
	return nil
}

// List implements provider.Provisioner.
func (p *Provisioner) List(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	names, err := p.prov.List()
	if err != nil {
		return nil, fmt.Errorf("listing kind clusters: %w", err)
	}
	return names, nil
}

// ExportKubeconfig implements provider.Provisioner. The exported file is
// verified private before it is considered written.
func (p *Provisioner) ExportKubeconfig(ctx context.Context, name string, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.prov.ExportKubeConfig(name, path, false); err != nil {
		return fmt.Errorf("exporting kubeconfig for %s: %w", name, err)
	}
	if err := verifyKubeconfig(path); err != nil {
		return err
	}
	return nil
}

// LoadImage implements provider.ImageLoader (the optional capability; the
// kind runtime can import images without a registry). It is the
// library-driven port of `kind load docker-image`: resolve the image's ID
// on the local daemon, docker save it into a tar once, then import that
// tar with containerd on every node that does not already carry the image
// under the exact reference. The import path (nodeutils.LoadImageArchive)
// is the same one kind's own load command uses, which is why an image
// loaded here is indistinguishable from one loaded by the CLI. An image
// whose ID is present on a node under the reference is skipped, so
// re-running a create that re-loads the same image is a no-op.
func (p *Provisioner) LoadImage(ctx context.Context, cluster string, imageRef string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	imageID, err := localImageID(imageRef)
	if err != nil {
		return fmt.Errorf("loading image %s into cluster %s: not present on the local Docker daemon: %w", imageRef, cluster, err)
	}
	nodeList, err := p.prov.ListInternalNodes(cluster)
	if err != nil {
		return fmt.Errorf("loading image %s: listing the nodes of cluster %s: %w", imageRef, cluster, err)
	}
	if len(nodeList) == 0 {
		return fmt.Errorf("loading image %s: no kind nodes found for cluster %q", imageRef, cluster)
	}
	var needing []nodes.Node
	for _, node := range nodeList {
		// The reverse lookup of the image ID's tags on the node: an
		// exact reference match means the image is already loaded there.
		// Any error means the lookup could not run (an unknown image ID
		// included), which the load path resolves.
		if tags, err := nodeutils.ImageTags(node, imageID); err == nil && tags[imageRef] {
			continue
		}
		needing = append(needing, node)
	}
	if len(needing) == 0 {
		return nil
	}
	tar, err := os.CreateTemp("", "kind-ocp-image-*.tar")
	if err != nil {
		return fmt.Errorf("loading image %s: %w", imageRef, err)
	}
	tarPath := tar.Name()
	defer func() { _ = os.Remove(tarPath) }()
	if err := tar.Close(); err != nil {
		return fmt.Errorf("loading image %s: %w", imageRef, err)
	}
	if err := dockerSave(ctx, imageRef, tarPath); err != nil {
		return fmt.Errorf("loading image %s: %w", imageRef, err)
	}
	for _, node := range needing {
		f, err := os.Open(tarPath)
		if err != nil {
			return fmt.Errorf("loading image %s into node %s: %w", imageRef, node.String(), err)
		}
		err = nodeutils.LoadImageArchive(node, f)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("loading image %s into node %s: %w (was the image built for the node's platform?)", imageRef, node.String(), err)
		}
	}
	return nil
}

// BuildImage implements provider.ImageBuilder (the optional
// capability the framework binds as the layers' image-building
// service). It runs one docker build against the daemon every other
// capability already drives: the same daemon LoadImage later imports
// the built image from, so the reference this tags is the one the
// cluster's nodes receive. --provenance=false keeps the built image a
// plain single-platform image regardless of the daemon's image store,
// so the save/import path LoadImage loads through stays the simple
// one. The flag is this implementation's choice, not the boundary's:
// a podman implementation would pass its own.
func (p *Provisioner) BuildImage(ctx context.Context, ref, contextDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ref == "" || contextDir == "" {
		return fmt.Errorf("building an image: no reference or no context directory")
	}
	run := p.dockerRun()
	if _, err := run(ctx, "build", "--provenance=false", "-t", ref, contextDir); err != nil {
		return fmt.Errorf("building the image %s from %s: %w", ref, contextDir, err)
	}
	return nil
}

// ProbeTCPFromNode implements provider.TCPProber (the optional
// capability the cross-cluster reachability check uses as its client).
// It connects from the cluster's control-plane node container: one
// context-aware Docker lookup resolves the node by kind's cluster and
// role labels, and one non-privileged docker exec runs the connect —
// a TCP connect needs no privileges, and kind's own node exec path
// always adds them. Both Docker invocations run under ctx
// (exec.CommandContext kills the CLI when ctx ends), so a stalled
// Docker call cannot outlive the probe's deadline as a live process.
// The connect is bounded inside the container too: each attempt runs
// under coreutils timeout in its own bash, so a silently-dropping
// endpoint is cut off at two seconds instead of blocking on the
// kernel's SYN retries.
//
// The typed endpoint never reaches a shell: the script is a static
// constant, and the address and port arrive as its own argv elements —
// bash's quoted positional parameters — so no byte of them is ever
// parsed as shell language. Scoped addresses and out-of-range ports
// are refused before Docker is invoked at all.
func (p *Provisioner) ProbeTCPFromNode(ctx context.Context, cluster string, addr netip.Addr, port int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateProbeEndpoint(addr, port); err != nil {
		return err
	}
	run := p.dockerRun()
	containerID, err := controlPlaneContainerID(ctx, run, cluster)
	if err != nil {
		return err
	}
	endpoint := netip.AddrPortFrom(addr, uint16(port))
	// The exec's stdout carries the script's "connected" line — nothing
	// the adapter parses; a failure is what the error is for.
	if _, err := run(ctx, "exec", containerID, "bash", "-c", connectScript, "connect", addr.String(), strconv.Itoa(port)); err != nil {
		return fmt.Errorf("connecting to %s from the control-plane node of cluster %s: %w",
			endpoint, cluster, err)
	}
	return nil
}

// ControlPlaneAPIEndpoints implements
// provider.ControlPlaneAPIEndpointResolver: the endpoints the runtime
// itself knows for the cluster's apiserver — the control-plane node's
// addresses on the network Docker recorded the node created on, each
// with the port kind configured that apiserver to listen on inside
// the node (the kubeadm bind port, one unexported constant for every
// cluster and every network setup kind creates). The addresses come
// from the same label-filtered node lookup the probe connects through
// (one inspect of its own, here, resolves them), so the endpoint the
// check corroborates against and the node the connect runs on are one
// container, and a cluster whose own kubernetes service endpoint
// disagrees with one of these is one the check refuses (the endpoint
// is stale, tampered with, or not a kind creation), never one it
// probes anyway. The whole resolution is bounded by ctx.
func (p *Provisioner) ControlPlaneAPIEndpoints(ctx context.Context, cluster string) ([]netip.AddrPort, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	addrs, err := p.controlPlaneNodeAddresses(ctx, cluster)
	if err != nil {
		return nil, err
	}
	endpoints := make([]netip.AddrPort, 0, len(addrs))
	for _, addr := range addrs {
		endpoints = append(endpoints, netip.AddrPortFrom(addr, kindAPIServerInternalPort))
	}
	return endpoints, nil
}

// kindAPIServerInternalPort mirrors kind's
// cluster/internal/providers/common.APIServerInternalPort: the port
// kind configures kubeadm's local API endpoint (bindPort) with on
// every cluster it creates — the listener inside the node that the
// cluster's own kubernetes service endpoint points at. kind keeps the
// constant in an internal package (unimportable from this module), so
// it is mirrored like kind's node labels are: kind's own behavior —
// every cluster it creates gets this bind port — is the contract. The
// mirror's live pin is the cross-cluster check's endpoint
// corroboration: a value that disagreed with what the pinned kind
// actually configures would fail every target's preparation on a real
// cluster, loudly, not pass silently.
const kindAPIServerInternalPort = 6443

// controlPlaneNodeAddresses returns the addresses Docker itself
// reports for the cluster's control-plane node container on the
// network kind's clusters share — the node's identity on that
// network, which is what the cross-cluster check corroborates its
// cluster-read endpoint address against (what a cluster's own API
// reports is writable by whoever can edit the reporting objects). The
// lookup is the same label-filtered one the probe connects through, so
// the address and the connect talk about one node. Which network that
// is comes from Docker's own record, never from this process's
// environment: kind passes the shared network to Docker as --net when
// it creates the node, and Docker keeps that record
// (HostConfig.NetworkMode) unchanged however many more attachments
// are added later — so the recorded name is the network the
// provisioner attached the node to, whatever environment a later
// command runs under (the override variable kind read at create time
// is invisible here on purpose: re-deriving it from the verify shell
// can only ever guess). The same one-inspect read also returns the
// container's whole per-network attachment map, and only the recorded
// network's entry is kept: an address on any other attachment need
// not identify the same node from a peer on the recorded network, so
// a side network's address never corroborates. The recorded network
// must resolve to exactly one attachment — a node Docker reports
// without a recorded network, without the recorded attachment, or
// whose inspect output does not decode is an error, never a guess,
// with no fallback to the environment or to the first attachment —
// and within the entry every non-empty field must parse and at least
// one address must result.
func (p *Provisioner) controlPlaneNodeAddresses(ctx context.Context, cluster string) ([]netip.Addr, error) {
	run := p.dockerRun()
	containerID, err := controlPlaneContainerID(ctx, run, cluster)
	if err != nil {
		return nil, err
	}
	out, err := run(ctx, "inspect", "--format", nodeNetworksFormat, containerID)
	if err != nil {
		return nil, fmt.Errorf("reading the addresses of the control-plane node of cluster %s: %w", cluster, err)
	}
	var inspect dockerNodeNetworks
	if err := json.Unmarshal(out, &inspect); err != nil {
		return nil, fmt.Errorf("reading the addresses of the control-plane node of cluster %s: the docker inspect network fields do not decode: %w", cluster, err)
	}
	if inspect.CreateNetwork == "" {
		return nil, fmt.Errorf("reading the addresses of the control-plane node of cluster %s: docker reports no network it was created on (it is attached to: %s)",
			cluster, describeNetworkNames(inspect.Networks))
	}
	attachment, attached := inspect.Networks[inspect.CreateNetwork]
	if !attached {
		return nil, fmt.Errorf("the control-plane node of cluster %s was created on network %q but is not attached to it (it is attached to: %s)",
			cluster, inspect.CreateNetwork, describeNetworkNames(inspect.Networks))
	}
	var addrs []netip.Addr
	for _, field := range []string{attachment.IPv4, attachment.IPv6} {
		if field == "" {
			continue
		}
		addr, err := netip.ParseAddr(field)
		if err != nil {
			return nil, fmt.Errorf("parsing an address of the control-plane node of cluster %s on network %q: %q is not an IP: %w", cluster, inspect.CreateNetwork, field, err)
		}
		addrs = append(addrs, addr)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("the control-plane node of cluster %s reports no address on the network it was created on (%q)", cluster, inspect.CreateNetwork)
	}
	return addrs, nil
}

// dockerNodeNetworks is what the one inspect in
// controlPlaneNodeAddresses decodes: the network Docker recorded when
// the node was created, and the container's whole per-network
// attachment map.
type dockerNodeNetworks struct {
	// CreateNetwork is Docker's HostConfig.NetworkMode: the value of
	// the --net the container was created with. Kind creates every
	// node with --net <the shared network>, and Docker leaves the
	// record unchanged when further attachments are added later, so
	// this is the provisioner's own choice of shared network, as the
	// daemon recorded it — not something this process re-derives.
	CreateNetwork string `json:"mode"`
	// Networks is the per-network attachment map keyed by network
	// name. Each entry carries the two address fields the
	// corroboration reads, under the names Docker itself emits.
	Networks map[string]dockerNetworkAttachment `json:"networks"`
}

// dockerNetworkAttachment is one entry of the docker inspect network
// map: the two address fields the corroboration reads, under the
// names Docker itself emits for them.
type dockerNetworkAttachment struct {
	IPv4 string `json:"IPAddress"`
	IPv6 string `json:"GlobalIPv6Address"`
}

// nodeNetworksFormat is the docker inspect format that emits, as one
// JSON object, the network the container was created on and its
// per-network attachment map keyed by network name. Reading it as
// JSON — instead of kind's comma-joined template over every network —
// keeps each address attributable to exactly one network: no
// delimiter can merge one network's IPv6 with the next network's
// IPv4. Selecting the entry by the recorded create network — rather
// than by a name this process computes from its own environment —
// makes the corroboration read exactly the network the provisioner
// attached the node to, in the same single Docker call.
const nodeNetworksFormat = `{"mode":{{json .HostConfig.NetworkMode}},"networks":{{json .NetworkSettings.Networks}}}`

// describeNetworkNames renders the attached network names for the
// not-attached refusal, sorted so the message is stable.
func describeNetworkNames(networks map[string]dockerNetworkAttachment) string {
	names := make([]string, 0, len(networks))
	for name := range networks {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// controlPlaneContainerID resolves a cluster's control-plane node
// container through kind's labels: one context-aware docker ps whose
// filters are exactly kind's cluster and role labels — the same node
// the probe connects through and the address resolution reads.
func controlPlaneContainerID(ctx context.Context, run dockerRunner, cluster string) (string, error) {
	out, err := run(ctx, "ps",
		"--filter", "label="+kindClusterLabel+"="+cluster,
		"--filter", "label="+kindNodeRoleLabel+"="+constants.ControlPlaneNodeRoleValue,
		"--format", "{{.ID}}")
	if err != nil {
		return "", fmt.Errorf("looking up the control-plane container of cluster %s: %w", cluster, err)
	}
	containerID := firstLine(out)
	if containerID == "" {
		return "", fmt.Errorf("no running control-plane container found for kind cluster %q", cluster)
	}
	return containerID, nil
}

// The Docker labels kind's provider puts on every node container,
// mirrored here because kind keeps them in an internal package. They
// are kind's stable public contract (every kind version that labels
// differently would break its own node listing).
const (
	kindClusterLabel  = "io.x-k8s.kind.cluster"
	kindNodeRoleLabel = "io.x-k8s.kind.role"
)

// connectScript is the node-side connect: a bounded retry loop around
// bash's /dev/tcp — no image preloaded on a kind node carries a
// connect tool (the node images are distroless), so the shell builtin
// is the client. The retry window exists because kube-proxy needs a
// beat to program a freshly created NodePort. The endpoint is not in
// the script: the caller passes the address and port as argv after it
// (bash -c script name addr port), where they are "$1" and "$2" —
// quoted, so word splitting cannot tear them apart either. Each
// attempt is its own bash under coreutils timeout, which is also what
// makes the bound reach the connect itself: the blocked connect is
// timeout's direct child, not a subshell of the looping shell.
const connectScript = `for i in $(seq 1 20); do
	if timeout 2 bash -c 'exec 3<>/dev/tcp/"$1"/"$2"' connect "$1" "$2" 2>/dev/null; then
		echo connected
		exit 0
	fi
	sleep 1
done
echo "could not connect to $1:$2"
exit 1`

// validateProbeEndpoint refuses endpoints that cannot be probed
// safely, before Docker is invoked at all: a scoped address (its zone
// is a link-local routing artifact, never a routable node address,
// and its text is arbitrary — the one part of a netip.Addr that is)
// and a port outside the valid range. Past this point the adapter may
// assume a bare, routable, canonically-rendered address.
func validateProbeEndpoint(addr netip.Addr, port int) error {
	if addr.Zone() != "" {
		return fmt.Errorf("refusing to connect to the scoped address %s: a zone is not a routable node address", addr)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("refusing to connect to port %d: outside the valid range 1-65535", port)
	}
	return nil
}

// dockerRunner runs one Docker CLI invocation under ctx and returns
// its stdout — the stream the adapter parses (docker ps prints IDs
// there; Docker's warnings, a config advice among them, go to stderr
// and must not be parsed as data). A failed invocation's stderr is
// carried in the returned error. It is a Provisioner field so the
// adapter's Docker boundary is testable without a daemon: the
// regressions inject a recorder and assert the exact argv, without
// executing anything.
type dockerRunner func(ctx context.Context, args ...string) ([]byte, error)

// runDocker is the production dockerRunner. Stdout and stderr are
// kept apart: the data the adapter parses comes from stdout alone,
// and a failed call's stderr is appended to the error for context.
func runDocker(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return out, fmt.Errorf("%w: %s", err, commandTail(exitErr.Stderr))
		}
	}
	return out, err
}

// dockerRun returns the injected Docker runner, or the real one.
func (p *Provisioner) dockerRun() dockerRunner {
	if p.docker != nil {
		return p.docker
	}
	return runDocker
}

// firstLine returns the first non-empty line of a command's output:
// the shape docker ps --format writes (one ID per line).
func firstLine(out []byte) string {
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// localImageID resolves an image reference to its local daemon's ID —
// the content identity kind load compares nodes against.
func localImageID(imageRef string) (string, error) {
	out, err := exec.Command("docker", "image", "inspect", "-f", "{{ .Id }}", imageRef).Output()
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", errors.New("docker image inspect returned no ID")
	}
	return id, nil
}

// dockerSave writes one image to dest as a tar, the archive
// LoadImageArchive imports on the nodes.
func dockerSave(ctx context.Context, imageRef, dest string) error {
	cmd := exec.CommandContext(ctx, "docker", "save", "-o", dest, imageRef)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker save %s: %w: %s", imageRef, err, commandTail(out))
	}
	return nil
}

// commandTail renders the tail of a failed command's output, on one
// line: enough to name the cause without dumping a whole build log.
func commandTail(out []byte) string {
	s := strings.Join(strings.Fields(string(out)), " ")
	if len(s) > 400 {
		s = "…" + s[len(s)-400:]
	}
	return s
}

func (p *Provisioner) exists(name string) (bool, error) {
	names, err := p.prov.List()
	if err != nil {
		return false, fmt.Errorf("listing kind clusters: %w", err)
	}
	for _, n := range names {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}

// verifyKubeconfig verifies what kind actually wrote, without repairing
// it: kind merges into an existing destination and creates missing
// parent directories 0755, so what kind produced — not what it was asked
// to produce — is what must pass the private, caller-owned, non-symlink,
// no-ACL checks before the kubeconfig is used. There is deliberately no
// corrective chmod: a chmod through a path that has not verified would
// itself be a write to an unverified file (the state-level preflight in
// store.KubeconfigPath has already refused unsafe destinations before
// kind was invoked), and kind creates new files at 0600, so anything
// that fails here is refused, not fixed.
func verifyKubeconfig(path string) error {
	if path == "" {
		return nil
	}
	if err := fscheck.CheckPrivateFile(path); err != nil {
		return fmt.Errorf("kubeconfig %s is not a private, caller-owned file: %w", path, err)
	}
	if err := fscheck.CheckManagedDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("kubeconfig directory for %s: %w", path, err)
	}
	return nil
}

// clusterConfig builds the kind v1alpha4 config for one control-plane node.
func clusterConfig(opts provider.CreateOptions) *v1alpha4.Cluster {
	return &v1alpha4.Cluster{
		TypeMeta: v1alpha4.TypeMeta{Kind: "Cluster", APIVersion: "kind.x-k8s.io/v1alpha4"},
		Name:     opts.Name,
		Nodes: []v1alpha4.Node{{
			Role:  v1alpha4.ControlPlaneRole,
			Image: opts.NodeImage,
		}},
	}
}

func dockerAvailable() error {
	cmd := exec.Command("docker", "info")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker CLI or daemon not available: %v (%s)", err, string(out))
	}
	return nil
}

// kindLogger adapts *slog.Logger to kind's logging interface, so kind's
// progress (image pull, node startup, readiness) is visible during create.
// kind's default is a no-op logger.
type kindLogger struct{ log *slog.Logger }

func (a kindLogger) Warn(msg string)                  { a.log.Warn(msg) }
func (a kindLogger) Warnf(format string, args ...any) { a.log.Warn(fmt.Sprintf(format, args...)) }
func (a kindLogger) Error(msg string)                 { a.log.Error(msg) }
func (a kindLogger) Errorf(format string, args ...any) {
	a.log.Error(fmt.Sprintf(format, args...))
}

// kindLevelInfo is kind's level for user-facing progress messages; higher
// levels are debug (V(1)) and trace (V(2)+) detail.
const kindLevelInfo = log.Level(0)

// V implements kind's log.Logger. kind logs its user-facing progress at
// level 0 and below, but its verbose detail only above: the raw kubeadm
// config (which contains the bootstrap token) at V(2) and the kubeadm
// init/join output at V(3). The adapter therefore enables level 0 and
// below, and discards everything above: ordinary progress stays visible
// while credential material never reaches the default log.
func (a kindLogger) V(level log.Level) log.InfoLogger {
	if level <= kindLevelInfo {
		return kindInfoLogger(a)
	}
	return log.NoopInfoLogger{}
}

// kindInfoLogger implements kind's leveled info logging over slog.
type kindInfoLogger struct{ log *slog.Logger }

func (a kindInfoLogger) Enabled() bool                    { return true }
func (a kindInfoLogger) Info(msg string)                  { a.log.Info(msg) }
func (a kindInfoLogger) Infof(format string, args ...any) { a.log.Info(fmt.Sprintf(format, args...)) }

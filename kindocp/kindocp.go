// Package kindocp is the façade over the provisioning loop: it creates N
// clusters, applies the configured layers, and verifies the result. The CLI
// is a thin skin over this package; external Go programs can use it directly
// for the same behavior.
//
// New supplies the shipped defaults (builtin layer registry, kind
// provisioner); Options overrides either for tests or alternate runtimes.
package kindocp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/stackrox/kind-ocp/config"
	"github.com/stackrox/kind-ocp/internal/builtin"
	"github.com/stackrox/kind-ocp/internal/kube"
	kindprov "github.com/stackrox/kind-ocp/internal/provider/kind"
	"github.com/stackrox/kind-ocp/internal/state"
	"github.com/stackrox/kind-ocp/layers"
	"github.com/stackrox/kind-ocp/provider"
)

// Options configures a Client. Config is required. Registry and Provisioner
// default to the shipped implementations (the builtin layer registry and the
// kind-as-library provisioner, which requires Docker); either can be
// overridden for tests or alternate runtimes.
type Options struct {
	Config      *config.Config
	Registry    *layers.Registry
	Provisioner provider.Provisioner
	Log         *slog.Logger
}

// Client drives cluster lifecycle for one resolved configuration.
type Client struct {
	// cfg is a snapshot of the caller's Config taken at construction, so
	// later mutations of the caller's copy cannot re-scope a live Client.
	cfg   *config.Config
	reg   *layers.Registry
	prov  provider.Provisioner
	log   *slog.Logger
	store *state.Store

	// clientset builds a kubernetes client from a kubeconfig path. It is a
	// field so tests can substitute a fake clientset; production callers
	// get kube.NewClientset — the one typed constructor internal/kube
	// builds, and it refuses redirects in the transport, so the lifecycle
	// waits, the identity reads, and the cross-cluster check's creates
	// and UID-preconditioned cleanup deletes can never be re-sent to an
	// endpoint a redirect chose.
	clientset func(kubeconfigPath string) (kubernetes.Interface, error)
}

// New builds a Client and opens the per-prefix state store. The prefix is
// validated even for library-constructed Configs, so an unvalidated prefix
// can never reach a filesystem join under the state root. The Config is
// snapshotted (config.Config.Clone): the caller may freely reuse or mutate
// its copy afterwards without re-scoping this client.
func New(opts Options) (*Client, error) {
	if opts.Config == nil {
		return nil, errors.New("kindocp: Config is required")
	}
	cfg := opts.Config.Clone()
	if err := config.ValidatePrefix(cfg.Prefix); err != nil {
		return nil, fmt.Errorf("kindocp: %w", err)
	}
	if cfg.StateRoot == "" {
		return nil, errors.New("kindocp: Config.StateRoot is required")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Registry == nil {
		opts.Registry = builtin.NewRegistry()
	}
	if opts.Provisioner == nil {
		prov, err := kindprov.New(opts.Log)
		if err != nil {
			return nil, err
		}
		opts.Provisioner = prov
	}
	return &Client{
		cfg:       cfg,
		reg:       opts.Registry,
		prov:      opts.Provisioner,
		log:       opts.Log,
		store:     state.Open(filepath.Join(cfg.StateRoot, cfg.Prefix)),
		clientset: kube.NewClientset,
	}, nil
}

// StateRoot returns the per-prefix state directory path.
func (c *Client) StateRoot() string { return c.store.Root() }

// Record is the create result for one cluster: what kind-ocp provisioned
// and applied. It mirrors the persisted state record without exposing the
// storage layout.
type Record struct {
	Name          string    `json:"name"`
	Context       string    `json:"context"`
	Profile       string    `json:"profile"`
	Layers        []string  `json:"layers"`
	KindNodeImage string    `json:"kindNodeImage"`
	Status        string    `json:"status"` // RecordStatusPending or RecordStatusComplete
	CreatedAt     time.Time `json:"createdAt"`
	// InstanceID identifies the cluster instance (the kube-system namespace
	// UID). A create run binds it once the cluster's API answers, before
	// any layer runs, so it is present on every complete record and on
	// pending records whose run died mid-layer. It is empty only while a
	// create is still converging the cluster or was interrupted before the
	// API answered; such records are never trusted for reuse. It changes
	// when a cluster is deleted and re-created under the same name.
	InstanceID string `json:"instanceId,omitempty"`
}

// Record statuses.
const (
	// RecordStatusPending marks a cluster whose create run did not finish:
	// it was provisioned and matches the recorded profile, but the layers
	// may be partially applied.
	RecordStatusPending = "pending"
	// RecordStatusComplete marks a cluster whose create run applied every
	// layer.
	RecordStatusComplete = "complete"
)

func fromStateRecord(r state.Record) Record {
	return Record{
		Name:          r.Name,
		Context:       r.Context,
		Profile:       r.Profile,
		Layers:        r.Layers,
		KindNodeImage: r.KindNodeImage,
		Status:        r.Status,
		CreatedAt:     r.CreatedAt,
		InstanceID:    r.InstanceID,
	}
}

// Create creates every configured cluster and applies its layers:
// cluster 1 alone first, then clusters 2..N concurrently (per-cluster
// state isolation — each cluster's kubeconfig, record, and layer
// application touch only its own paths). On failure the error names
// each failing cluster and step; clusters are left in place for
// debugging (AC-1.7). The concurrent wave runs to completion and
// aggregates: an error in one cluster neither cancels the others
// mid-layer nor hides their outcomes behind the first failure.
//
// Cluster 1 runs first by itself because its create establishes the
// shared kind network and pulls the pinned node image — the two
// cluster-scoped resources every later cluster depends on — so the
// concurrent wave never races on either. A failure there stops the run
// before any other cluster starts, which is the sequential contract
// every earlier increment ran under.
//
// Every cluster's profile is resolved before the first cluster is
// provisioned, so an unknown profile or an override of an ungenerated
// cluster name fails before any cluster exists. An existing cluster is
// reused only when its state record matches the requested node image and
// profile and the live cluster is the instance the record is bound to;
// anything else — including an unbound record from an interrupted create,
// whose live cluster cannot be proven to be the recorded one — is rejected
// with instructions to delete and re-create, because kind-ocp never
// removes or upgrades applied layers.
func (c *Client) Create(ctx context.Context) ([]Record, error) {
	plans, err := c.planClusters()
	if err != nil {
		return nil, err
	}
	// Cancellation is checked before any side effect runs, the state
	// boundary included: a run that never started must not touch
	// anything.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The store root is ensured — and the state boundary verified —
	// once, before the fan-out: concurrent per-cluster walks then only
	// ever find an already-marked root (the discipline state.walk
	// prescribes for concurrent creation), and the host tune below
	// runs only after the boundary has been verified: a run whose
	// state root is refused must not have changed host settings on its
	// way to failing.
	if _, err := c.store.EnsureRoot(); err != nil {
		return nil, err
	}
	// Persist what this run set out to build — the number of clusters,
	// in the state marker — before anything is provisioned. Verify's
	// whole-prefix count assertion reads that expectation instead of
	// resolving the create-only count itself (§3.3: the CLI reads no
	// create-only values outside create, so a plain verify under a
	// different resolved count cannot drift the assertion), and an
	// interrupted create leaves the expectation behind: the shortfall
	// is reported by the next verify, not silently dropped with the
	// cluster.
	if err := c.store.SetExpectedClusters(len(plans)); err != nil {
		return nil, err
	}
	// Once per run, before anything is provisioned: the host tune the
	// runtime asks for (kind's node containers need the inotify
	// instances), best-effort and never fatal. Only a provisioner that
	// implements the host-limit capability is asked — the real kind
	// runtime — so a test double or a remote runtime never changes host
	// settings. Cancellation and the state boundary have both been
	// checked above; the raise itself re-checks cancellation before
	// any write.
	if raiser, ok := c.prov.(provider.HostLimitRaiser); ok {
		raiser.RaiseHostLimits(ctx)
	}

	// records and errs are indexed by plan position, so no goroutine
	// shares a slice header and the result order is the plan order
	// regardless of completion order.
	records := make([]Record, len(plans))
	errs := make([]error, len(plans))

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Cluster 1, sequential-first (see the doc comment).
	rec, err := c.createCluster(ctx, plans[0])
	if err != nil {
		return nil, fmt.Errorf("cluster %s: %w", plans[0].name, err)
	}
	records[0] = fromStateRecord(rec)

	// Clusters 2..N, concurrently.
	var wg sync.WaitGroup
	for i := 1; i < len(plans); i++ {
		wg.Add(1)
		go func(i int, plan clusterPlan) {
			defer wg.Done()
			rec, err := c.createCluster(ctx, plan)
			if err != nil {
				// Reported, not returned early: the wave runs to
				// completion so every cluster's outcome is known and
				// no in-flight layer application is cancelled out
				// from under its record.
				errs[i] = fmt.Errorf("cluster %s: %w", plan.name, err)
				return
			}
			records[i] = fromStateRecord(rec)
		}(i, plans[i])
	}
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		// The successful clusters are still reported alongside the
		// error, as the sequential loop did for the clusters before
		// the failing one.
		return nonNilRecords(records), err
	}
	return records, nil
}

func nonNilRecords(records []Record) []Record {
	out := make([]Record, 0, len(records))
	for _, rec := range records {
		if rec.Name != "" {
			out = append(out, rec)
		}
	}
	return out
}

// clusterPlan is one cluster's fully-resolved create intent.
type clusterPlan struct {
	name       string
	profile    string
	layerNames []string
}

// planClusters validates the create-only configuration and resolves every
// cluster's profile up front. A mistake in the last cluster's profile must
// not leave the earlier clusters behind.
func (c *Client) planClusters() ([]clusterPlan, error) {
	if err := c.cfg.ValidateCreate(); err != nil {
		return nil, err
	}
	names := c.cfg.ClusterNames()
	plans := make([]clusterPlan, 0, len(names))
	for _, name := range names {
		profile := c.cfg.ProfileFor(name)
		layerNames, err := layers.LayerNamesForProfile(c.reg, profile, c.cfg.Profiles)
		if err != nil {
			return nil, fmt.Errorf("cluster %s: resolving profile %q: %w", name, profile, err)
		}
		plans = append(plans, clusterPlan{name: name, profile: profile, layerNames: layerNames})
	}
	return plans, nil
}

func (c *Client) createCluster(ctx context.Context, plan clusterPlan) (state.Record, error) {
	log := c.log.With("cluster", plan.name)

	kubeconfigPath, err := c.store.KubeconfigPath(plan.name)
	if err != nil {
		return state.Record{}, err
	}

	created, err := c.prov.Create(ctx, provider.CreateOptions{
		Name:           plan.name,
		NodeImage:      c.cfg.KindNodeImage,
		KubeconfigPath: kubeconfigPath,
	})
	if err != nil {
		return state.Record{}, fmt.Errorf("provisioning kind cluster: %w", err)
	}

	createdAt := time.Now().UTC()
	// previousID is the instance ID the existing record is bound to. It is
	// empty on the fresh-create path and non-empty on every reuse (the
	// reuse check rejects unbound records), so a retry rewrites its
	// pending record without ever losing the binding.
	var previousID string
	if created {
		log.Info("created kind cluster")
	} else {
		log.Info("reusing existing kind cluster")
		previous, err := c.checkReuseCompatible(ctx, plan, kubeconfigPath)
		if err != nil {
			return state.Record{}, err
		}
		if !previous.CreatedAt.IsZero() {
			createdAt = previous.CreatedAt
		}
		previousID = previous.InstanceID
	}

	rec := state.Record{
		Name:          plan.name,
		Context:       "kind-" + plan.name,
		Profile:       plan.profile,
		Layers:        plan.layerNames,
		KindNodeImage: c.cfg.KindNodeImage,
		Status:        state.StatusPending,
		CreatedAt:     createdAt,
		InstanceID:    previousID,
	}
	// The pending record marks the cluster as managed with this image and
	// profile before anything slow runs. A create that dies mid-layer
	// leaves an honest "pending" marker (not an unmanaged cluster), and a
	// retry of the same instance reconciles from it. On the reuse path it
	// carries the existing binding; on the fresh path it is unbound until
	// the API answers below, and an unbound record is never re-adopted.
	if err := c.store.WriteRecord(rec); err != nil {
		return state.Record{}, fmt.Errorf("writing state record: %w", err)
	}

	// One bounded, context-aware readiness check on both the created and the
	// reused path, before any layer touches the cluster. kind's internal
	// wait reports a timeout as success, so it is deliberately not used.
	if err := c.waitNodesReady(ctx, kubeconfigPath); err != nil {
		return state.Record{}, fmt.Errorf("waiting for nodes Ready: %w", err)
	}

	// The API just proved responsive; bind the record to this cluster
	// instance before any layer runs, so a same-named replacement can
	// never be mistaken for the recorded one — not even by a pending
	// record.
	instanceID, err := c.clusterInstanceID(ctx, kubeconfigPath)
	if err != nil {
		return state.Record{}, fmt.Errorf("identifying cluster instance: %w", err)
	}
	if previousID != "" && instanceID != previousID {
		// The cluster was replaced between the reuse check above and this
		// binding read. Adopting the replacement would apply the recorded
		// profile to a cluster kind-ocp knows nothing about.
		return state.Record{}, errors.New(
			"the cluster with this name was replaced while this run was converging it " +
				"(same name, different instance) — delete and re-create")
	}
	rec.InstanceID = instanceID
	if err := c.store.WriteRecord(rec); err != nil {
		return state.Record{}, fmt.Errorf("writing state record: %w", err)
	}

	env := layers.Env{
		ClusterName:    plan.name,
		ContextName:    rec.Context,
		KubeconfigPath: kubeconfigPath,
		Log:            log,
		Settings:       c.cfg.Layers,
	}
	for _, layerName := range plan.layerNames {
		l, ok := c.reg.Get(layerName)
		if !ok {
			// planClusters already validated this; guard anyway.
			return state.Record{}, fmt.Errorf("layer %s is not registered", layerName)
		}
		// The cache service is bound per layer: the callback a layer
		// receives can only address its own cache directory, under the
		// one pin it passes (layers.CacheDirFunc).
		env.CacheDir = c.store.CacheDirFor(layerName)
		// The image-loading service is bound per cluster, and only when
		// the provisioner carries the optional capability
		// (provider.ImageLoader): a runtime that cannot import images
		// leaves the service unset, and a layer that needs it refuses
		// to run rather than build an image nothing can run. The
		// image-building service (provider.ImageBuilder) is bound the
		// same way: the build itself is cluster-independent — it lands
		// on the daemon — but it rides the same per-cluster env so a
		// layer sees one binding rule for both halves of building and
		// loading a local image.
		env.BuildImage = nil
		env.LoadImage = nil
		if builder, ok := c.prov.(provider.ImageBuilder); ok {
			env.BuildImage = builder.BuildImage
		}
		if loader, ok := c.prov.(provider.ImageLoader); ok {
			clusterName := plan.name
			env.LoadImage = func(ctx context.Context, imageRef string) error {
				return loader.LoadImage(ctx, clusterName, imageRef)
			}
		}
		if err := l.Apply(ctx, env); err != nil {
			return state.Record{}, fmt.Errorf("layer %s: %w", layerName, err)
		}
		log.Info("applied layer", "layer", layerName)
	}

	rec.Status = state.StatusComplete
	// rec.InstanceID was bound above, before the layers ran; the completed
	// record keeps that binding.
	if err := c.store.WriteRecord(rec); err != nil {
		return state.Record{}, fmt.Errorf("writing state record: %w", err)
	}
	return rec, nil
}

// checkReuseCompatible rejects reusing an existing cluster that does not
// match what this run requests. kind-ocp never removes or upgrades applied
// layers, so a mismatched reuse would leave layers in place that verify no
// longer checks; the caller is directed to delete and re-create instead.
// The record must also be bound to the live cluster: a cluster deleted and
// re-created under the same name is a different instance, and a record that
// was never bound (its create run was interrupted before the identity was
// recorded) cannot prove anything about the cluster that now carries the
// name. Both are rejected; the cluster is kept, never deleted here.
func (c *Client) checkReuseCompatible(ctx context.Context, plan clusterPlan, kubeconfigPath string) (state.Record, error) {
	rec, found, err := c.store.Record(plan.name)
	if err != nil {
		return state.Record{}, err
	}
	if !found {
		return state.Record{}, errors.New(
			"cluster exists but is not managed by kind-ocp (no state record); " +
				"if it is a leftover, delete it first")
	}
	if rec.KindNodeImage != c.cfg.KindNodeImage {
		return state.Record{}, fmt.Errorf(
			"existing cluster was created with node image %q, requested %q; "+
				"kind-ocp does not upgrade nodes in place — delete and re-create",
			rec.KindNodeImage, c.cfg.KindNodeImage)
	}
	if rec.Profile != plan.profile || !slices.Equal(rec.Layers, plan.layerNames) {
		return state.Record{}, fmt.Errorf(
			"existing cluster has profile %q applied, requested %q; "+
				"kind-ocp does not add or remove layers on reuse — delete and re-create",
			rec.Profile, plan.profile)
	}
	if rec.InstanceID == "" {
		return state.Record{}, errors.New(
			"the state record for this cluster is not bound to a cluster instance " +
				"(its create run was interrupted before the cluster identity was recorded); " +
				"kind-ocp cannot prove the live cluster is the one it recorded — " +
				"delete and re-create")
	}
	live, err := c.clusterInstanceID(ctx, kubeconfigPath)
	if err != nil {
		return state.Record{}, fmt.Errorf("checking cluster identity: %w", err)
	}
	if live != rec.InstanceID {
		return state.Record{}, fmt.Errorf(
			"existing cluster has the same name but is a different instance than the one in state " +
				"(it was probably deleted and re-created outside kind-ocp) — delete and re-create")
	}
	return rec, nil
}

func (c *Client) waitNodesReady(ctx context.Context, kubeconfigPath string) error {
	cs, err := c.clientset(kubeconfigPath)
	if err != nil {
		return err
	}
	return kube.WaitForNodesReady(ctx, cs, c.cfg.Timeouts.NodeReady)
}

// clusterInstanceID reads the live cluster's instance identifier, bounded
// by the node-ready timeout so a stalled response cannot hang the caller.
func (c *Client) clusterInstanceID(ctx context.Context, kubeconfigPath string) (string, error) {
	cs, err := c.clientset(kubeconfigPath)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeouts.NodeReady)
	defer cancel()
	return kube.ClusterInstanceID(ctx, cs)
}

// Delete removes every cluster whose name has the exact generated shape
// <prefix>-<positive integer> — including clusters left over from a run with
// a larger count — and clears the whole per-prefix state directory. Names of
// other prefixes are never touched, including overlapping ones (prefix
// "team" does not match "team-dev-1"). Both halves are idempotent (AC-1.5):
// a create after a delete starts from zero state.
//
// The state boundary is checked before any lifecycle side effect: a
// per-prefix state directory that exists but is not verifiably kind-ocp
// state (wrong owner, non-private permissions, symlinks, or no state
// marker) must never be removed, so delete refuses to run at all rather
// than delete the clusters and strand unrecognized state behind them.
// The whole deletion is then pinned to the tree that check verified —
// the verified directory is held open for the deletion's lifetime and
// compared before every handoff and before cleanup, so state that
// replaces it at the same path mid-deletion (including a tree a
// recycling filesystem would hand the very same inode numbers) is a
// refusal, never a tree to write into or remove. An ancestor alias
// re-pointed or removed while the clusters are being deleted is a
// refusal before any further write — never a switch onto a different
// tree. When no state exists at the start, the first handoff prepares
// the tree — exclusively, so state that appears in between is a
// refusal rather than an adoption — and the deletion stays pinned to
// it, so cleanup removes only state this delete prepared. The held
// directory is released on every return path, success or failure.
func (c *Client) Delete(ctx context.Context) ([]string, error) {
	removal, err := c.store.RemovableRoot()
	if err != nil {
		return nil, fmt.Errorf("refusing to delete: %w", err)
	}
	defer func() { _ = removal.Close() }()
	clusters, err := c.prov.List(ctx)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, name := range clusters {
		if !config.IsGeneratedClusterName(c.cfg.Prefix, name) {
			continue
		}
		// The kubeconfig path is resolved and preflighted under the
		// tree this deletion is pinned to, before it reaches the
		// provisioner: kind strips the cluster's context from that
		// file, and kind's locking creates a missing parent
		// directory 0755 — which the boundary checks would then
		// (rightly) refuse. Remove clears whatever this had to
		// create.
		kubeconfigPath, err := removal.KubeconfigPath(name)
		if err != nil {
			return deleted, err
		}
		if err := c.prov.Delete(ctx, name, kubeconfigPath); err != nil {
			return deleted, err
		}
		deleted = append(deleted, name)
		c.log.Info("deleted kind cluster", "cluster", name)
	}
	if err := removal.Remove(); err != nil {
		return deleted, err
	}
	return deleted, nil
}

// ClusterInfo describes one prefix-scoped cluster for list output.
type ClusterInfo struct {
	Name    string   `json:"name"`
	Context string   `json:"context"`
	State   string   `json:"state"` // "running" or "missing"
	Managed bool     `json:"managed"`
	Status  string   `json:"status"` // record status: "complete" or "pending"
	Profile string   `json:"profile"`
	Layers  []string `json:"layers"`
}

// List returns the union of prefix-scoped clusters known to the provisioner
// and recorded in state, sorted by name.
func (c *Client) List(ctx context.Context) ([]ClusterInfo, error) {
	clusters, err := c.prov.List(ctx)
	if err != nil {
		return nil, err
	}

	recs, err := c.store.Records()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*ClusterInfo, len(recs))
	for _, rec := range recs {
		byName[rec.Name] = &ClusterInfo{
			Name:    rec.Name,
			Context: rec.Context,
			State:   "missing",
			Managed: true,
			Status:  rec.Status,
			Profile: rec.Profile,
			Layers:  rec.Layers,
		}
	}
	for _, name := range clusters {
		if !config.IsGeneratedClusterName(c.cfg.Prefix, name) {
			continue
		}
		info, ok := byName[name]
		if !ok {
			info = &ClusterInfo{Name: name, Context: "kind-" + name}
			byName[name] = info
		}
		info.State = "running"
	}

	out := make([]ClusterInfo, 0, len(byName))
	for _, info := range byName {
		out = append(out, *info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// CheckStatus is the outcome of one verify check.
type CheckStatus string

// Check outcomes.
const (
	StatusPass    CheckStatus = "PASS"
	StatusFail    CheckStatus = "FAIL"
	StatusSkipped CheckStatus = "SKIP"
)

// CheckResult is one line of verify output.
type CheckResult struct {
	Cluster string      `json:"cluster"`
	Check   string      `json:"check"`
	Status  CheckStatus `json:"status"`
	Detail  string      `json:"detail"`
}

// Verify runs the global checks and every recorded layer's Verify for the
// named clusters (all prefix-scoped clusters when names is empty). A
// failing check is reported as FAIL with detail; only structural problems
// (unknown cluster name, no clusters at all) return an error.
//
// The whole-prefix form (no names) is the audit form: it opens with the
// cluster-count assertion — every cluster the last create set out to
// build, per the expectation create persisted in the state marker, must
// be in the inventory — so a cluster whose container and state record
// both vanished FAILs instead of dropping out of the checks (see
// checkExpectedClusters). The named form keeps targeted semantics and
// runs no count assertion.
//
// Verify reports management state explicitly: an existing cluster without a
// state record FAILs "managed-state" instead of passing vacuously with zero
// layer checks, a record whose create run never finished FAILs
// "create-status", and a record that is not bound to the live cluster
// instance FAILs "instance-id". Clusters are selected by list, but every
// record-derived check (management status, create status, instance
// identity, context, layer list) reads one current record snapshot after
// selection, so a record removed or replaced mid-verify fails against
// current state instead of passing checks written for stale data.
func (c *Client) Verify(ctx context.Context, names []string) ([]CheckResult, error) {
	infos, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 {
		return nil, fmt.Errorf("no clusters with prefix %q (run create first)", c.cfg.Prefix)
	}
	if len(names) > 0 {
		byName := make(map[string]ClusterInfo, len(infos))
		for _, info := range infos {
			byName[info.Name] = info
		}
		var filtered []ClusterInfo
		selected := make(map[string]bool, len(names))
		for _, want := range names {
			info, ok := byName[want]
			if !ok {
				return nil, fmt.Errorf("unknown cluster %q (known: %s)", want, joinNames(infos))
			}
			if selected[want] {
				// A repeated name would reach the cross-cluster check
				// as two peers that are one cluster: the pair count
				// would be satisfied with nothing to connect to.
				return nil, fmt.Errorf("cluster %q is named more than once; name each cluster once", want)
			}
			selected[want] = true
			filtered = append(filtered, info)
		}
		infos = filtered
	}

	var results []CheckResult
	// The whole-prefix form is the audit form, so it opens with the
	// count assertion: every cluster the last create set out to build
	// must be in the inventory. A cluster whose container and state
	// record both vanished is invisible to every other check (list
	// selects from the inventory, and nothing else names it), and the
	// survivors alone could report all-PASS — the finding this check
	// exists to close. The named form keeps today's targeted semantics:
	// a recovery verify of one cluster must not fail on a sibling.
	if len(names) == 0 {
		results = append(results, c.checkExpectedClusters(infos)...)
	}
	var clusters []verifiedCluster
	for _, info := range infos {
		rs, vc := c.verifyCluster(ctx, info)
		results = append(results, rs...)
		clusters = append(clusters, vc)
	}
	// The cross-cluster check runs after the per-cluster checks, over
	// the clusters they verified: its sources and targets are exactly
	// the clusters whose identity checks passed (§3.6).
	results = append(results, c.checkCrossClusterNodePort(ctx, clusters)...)
	return results, nil
}

func (c *Client) verifyCluster(ctx context.Context, info ClusterInfo) ([]CheckResult, verifiedCluster) {
	log := c.log.With("cluster", info.Name)
	var results []CheckResult
	vc := verifiedCluster{name: info.Name}

	if info.State != "running" {
		results = append(results, CheckResult{
			Cluster: info.Name,
			Check:   "cluster-running",
			Status:  StatusFail,
			Detail:  "cluster does not exist in the provisioner (state record exists); delete and re-create",
		})
		return results, vc
	}

	// Make sure the kubeconfig exists in state even for clusters kind-ocp
	// did not create this run.
	kubeconfigPath, err := c.store.KubeconfigPath(info.Name)
	if err != nil {
		results = append(results, CheckResult{
			Cluster: info.Name,
			Check:   "kubeconfig",
			Status:  StatusFail,
			Detail:  err.Error(),
		})
		return results, vc
	}
	vc.kubeconfigPath = kubeconfigPath
	if err := c.prov.ExportKubeconfig(ctx, info.Name, kubeconfigPath); err != nil {
		results = append(results, CheckResult{
			Cluster: info.Name,
			Check:   "kubeconfig",
			Status:  StatusFail,
			Detail:  err.Error(),
		})
		return results, vc
	}

	results = append(results, c.checkNodesReady(ctx, info, kubeconfigPath))

	// One current record snapshot drives every record-derived check below:
	// management status, create status, instance identity, context, and
	// the layer list. List only selected the cluster; a record removed or
	// replaced after selection must fail against current state, not pass
	// checks written for a snapshot — or, with the record gone, no checks
	// at all.
	rec, found, err := c.store.Record(info.Name)
	if err != nil {
		// list() read this record's directory moments ago; a read that
		// fails now must not silently skip the record-derived checks.
		results = append(results, CheckResult{
			Cluster: info.Name,
			Check:   "managed-state",
			Status:  StatusFail,
			Detail:  fmt.Sprintf("reading the state record: %v", err),
		})
		return results, vc
	}
	if !found {
		results = append(results, CheckResult{
			Cluster: info.Name,
			Check:   "managed-state",
			Status:  StatusFail,
			Detail:  "cluster exists but has no kind-ocp state record (unmanaged); delete it or re-create it with kind-ocp",
		})
		return results, vc
	}

	recordResults, stop := c.checkRecordIdentity(ctx, info.Name, rec, kubeconfigPath)
	results = append(results, recordResults...)
	if stop {
		return results, vc
	}
	// The record is bound to the live cluster instance: this cluster
	// may take part in the cross-cluster check, as source or target,
	// and the check's mutations stay bound to exactly this instance.
	vc.instanceID = rec.InstanceID

	// No CacheDir or LoadImage here, by design: the cache callback
	// creates directories and image loading mutates the cluster's
	// runtime, and Verify leaves no state behind. A layer whose Verify
	// needs either must fail (nil service) rather than create state or
	// load images during a verify run.
	env := layers.Env{
		ClusterName: info.Name,
		ContextName: rec.Context,
		// The mutating layer checks are bound to the instance
		// checkRecordIdentity just confirmed: the layer re-reads the
		// cluster's identity through its own clients and refuses to
		// mutate anything else, so a kubeconfig replaced between that
		// check and the layer's load cannot turn the probe into a
		// mutation of an unverified replacement.
		ClusterInstanceID: rec.InstanceID,
		KubeconfigPath:    kubeconfigPath,
		Log:               log,
		Settings:          c.cfg.Layers,
	}
	for _, layerName := range rec.Layers {
		l, ok := c.reg.Get(layerName)
		if !ok {
			results = append(results, CheckResult{
				Cluster: info.Name,
				Check:   "layer:" + layerName,
				Status:  StatusFail,
				Detail:  "layer is not registered in this kind-ocp build",
			})
			continue
		}
		result := CheckResult{Cluster: info.Name, Check: "layer:" + layerName, Status: StatusPass}
		if err := l.Verify(ctx, env); err != nil {
			result.Status = StatusFail
			result.Detail = err.Error()
		}
		results = append(results, result)
	}
	return results, vc
}

// checkRecordIdentity verifies that the record describes the live cluster,
// from the one snapshot the caller read after selecting the cluster. It
// produces:
//
//   - create-status FAIL when the record is pending (the create run did not
//     finish; a bound pending record can still be converged by re-running
//     create);
//   - instance-id FAIL, stopping further checks, when the record is not
//     bound to a cluster instance (its create run was interrupted before
//     the identity was recorded — the name alone proves nothing), when the
//     live identity cannot be read, or when the live cluster is a different
//     instance than the recorded one (it was probably deleted and
//     re-created outside kind-ocp).
//
// stop=true means the remaining layer checks are meaningless against this
// cluster; the caller must not run them.
func (c *Client) checkRecordIdentity(ctx context.Context, name string, rec state.Record, kubeconfigPath string) (results []CheckResult, stop bool) {
	if rec.Status == state.StatusPending {
		results = append(results, CheckResult{
			Cluster: name,
			Check:   "create-status",
			Status:  StatusFail,
			Detail:  "create did not complete for this cluster (layers may be partially applied); re-run create or delete it",
		})
	}
	if rec.InstanceID == "" {
		results = append(results, CheckResult{
			Cluster: name,
			Check:   "instance-id",
			Status:  StatusFail,
			Detail: "state record was never bound to the cluster's identity " +
				"(create was interrupted before the cluster identity was recorded); " +
				"the live cluster cannot be verified against it — delete and re-create",
		})
		return results, true
	}
	live, err := c.clusterInstanceID(ctx, kubeconfigPath)
	if err != nil {
		results = append(results, CheckResult{
			Cluster: name,
			Check:   "instance-id",
			Status:  StatusFail,
			Detail:  fmt.Sprintf("reading the live cluster identity: %v", err),
		})
		return results, true
	}
	if live != rec.InstanceID {
		results = append(results, CheckResult{
			Cluster: name,
			Check:   "instance-id",
			Status:  StatusFail,
			Detail: "state record describes a different cluster instance with this name " +
				"(the cluster was probably deleted and re-created outside kind-ocp); delete and re-create",
		})
		return results, true
	}
	return results, false
}

// expectedClustersCheck is the count assertion's name in verify output.
// Its line is prefix-scoped (the CLUSTER column carries the prefix, not
// a cluster name): it asserts a property of the whole prefix — the
// inventory matches what create built — and no cluster can be named the
// bare prefix (generated names are <prefix>-<integer>), so the line
// never collides with a cluster's own.
const expectedClustersCheck = "cluster-count"

// checkExpectedClusters asserts the audit form's completeness: every
// cluster the last create set out to build is in the inventory — the
// union of the provisioner's clusters and the state records, which is
// what list selected from. The expectation is what create persisted in
// the state marker, never the count this client happened to resolve:
// the CLI reads create-only values only for create, so a prefix created
// with --count 3 verified by a plain verify that would resolve count 2
// reports the create's truth (no drift, and no FAIL from the survivor
// set shrinking).
//
// The check degrades to a SKIP — never a false FAIL, and never blocking
// the clusters that exist — whenever the expectation cannot be asserted:
// a state root written by an older binary carries none, a marker that
// cannot be read is corrupt state (the reason is reported, the other
// checks still run), and a value no create could have written (zero or
// negative; create validates count ≥ 1 before persisting) is corruption
// of the same kind. Only one condition FAILs: a valid expectation with
// named clusters missing from both the provisioner and state — exactly
// the vanished-cluster case no other check can see. Extra prefix-scoped
// clusters beyond the expectation are not a FAIL: leftovers from a
// larger count are a supported state (create never removes them), and
// each still gets its own lines below.
func (c *Client) checkExpectedClusters(infos []ClusterInfo) []CheckResult {
	line := func(status CheckStatus, detail string) CheckResult {
		return CheckResult{Cluster: c.cfg.Prefix, Check: expectedClustersCheck, Status: status, Detail: detail}
	}
	expected, found, err := c.store.ExpectedClusters()
	switch {
	case err != nil:
		return []CheckResult{line(StatusSkipped, fmt.Sprintf(
			"reading the persisted cluster expectation: %v — the clusters that exist are still checked below", err))}
	case !found:
		return []CheckResult{line(StatusSkipped,
			"this state carries no persisted cluster expectation (written by an older kind-ocp) — "+
				"re-run create to record one; the clusters that exist are still checked below")}
	case expected < 1:
		return []CheckResult{line(StatusSkipped, fmt.Sprintf(
			"the persisted cluster expectation is not a count create could have written (%d) — "+
				"the state marker is corrupt; delete and re-create. The clusters that exist are still checked below",
			expected))}
	}
	live := make(map[string]bool, len(infos))
	for _, info := range infos {
		live[info.Name] = true
	}
	var missing []string
	for _, name := range config.GeneratedClusterNames(c.cfg.Prefix, expected) {
		if !live[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return []CheckResult{line(StatusPass, fmt.Sprintf(
			"all %d cluster(s) the last create expected are in the inventory", expected))}
	}
	return []CheckResult{line(StatusFail, fmt.Sprintf(
		"the last create expected %d cluster(s); missing from both the provisioner and kind-ocp state: %s — "+
			"re-run create to build the missing cluster(s) (clusters still present are reused when they match)",
		expected, strings.Join(missing, ", ")))}
}

func (c *Client) checkNodesReady(ctx context.Context, info ClusterInfo, kubeconfigPath string) CheckResult {
	result := CheckResult{Cluster: info.Name, Check: "nodes-ready"}

	cs, err := c.clientset(kubeconfigPath)
	if err != nil {
		result.Status = StatusFail
		result.Detail = err.Error()
		return result
	}
	// One API call, but bounded like every other: a stalled response must
	// fail the check, not hang verify.
	checkCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeouts.NodeReady)
	defer cancel()
	ready, notReady, err := kube.NodesReady(checkCtx, cs)
	switch {
	case err != nil:
		result.Status = StatusFail
		result.Detail = err.Error()
	case len(notReady) > 0:
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("nodes not Ready: %s", strings.Join(notReady, ", "))
	case ready == 0:
		// Zero nodes is a failure, not a vacuous pass.
		result.Status = StatusFail
		result.Detail = "no nodes found"
	default:
		result.Status = StatusPass
		result.Detail = fmt.Sprintf("%d node(s) Ready", ready)
	}
	return result
}

// Kubeconfig returns the path to a cluster's kubeconfig, exporting it from
// the live cluster when the state copy is missing. name must be a generated
// cluster name for this prefix; anything else is rejected before it can
// reach a state path.
//
// The cached copy is returned only after the state boundary and the file
// itself verify (store.CheckKubeconfig): a kubeconfig that is not a
// private, caller-owned regular file — a restored tree, a tampered cache —
// is refused instead of handed to kubectl. The returned path is the
// symlink-resolved path that was verified. The re-exported copy is
// verified the same way, and the verification must return exactly the
// path the export wrote: a state root that moved during the export
// (an ancestor alias re-pointed) means the verified file is some other
// tree's pre-existing cache — not what was just exported — so the
// answer is a refusal naming both trees, never a path into a tree the
// call did not export into.
func (c *Client) Kubeconfig(ctx context.Context, name string) (string, error) {
	if !config.IsGeneratedClusterName(c.cfg.Prefix, name) {
		return "", fmt.Errorf("unknown cluster %q (clusters for prefix %q are named %s-<i>)", name, c.cfg.Prefix, c.cfg.Prefix)
	}
	path, err := c.store.CheckKubeconfig(name)
	switch {
	case err == nil:
		return path, nil
	case errors.Is(err, fs.ErrNotExist):
		// No cached copy — re-export below.
	default:
		return "", err
	}
	live, err := c.prov.List(ctx)
	if err != nil {
		return "", err
	}
	found := slices.Contains(live, name)
	if !found {
		_, recFound, _ := c.store.Record(name)
		if !recFound {
			return "", fmt.Errorf("unknown cluster %q (no state record and no live cluster; prefix %q)", name, c.cfg.Prefix)
		}
	}
	path, err = c.store.KubeconfigPath(name)
	if err != nil {
		return "", err
	}
	if err := c.prov.ExportKubeconfig(ctx, name, path); err != nil {
		return "", err
	}
	// The export only counts when the state it wrote is the state this
	// call verified: with the state root moved mid-export, the
	// post-check verifies a different tree's pre-existing cache — not
	// what kind just wrote — and returning it would hand out a
	// kubeconfig this call never exported. There is no repair either:
	// re-exporting into the new tree would follow the moved root.
	verified, err := c.store.CheckKubeconfig(name)
	if err != nil {
		return "", fmt.Errorf("verifying the exported kubeconfig: %w", err)
	}
	if verified != path {
		return "", fmt.Errorf(
			"the state root moved during the export: kind-ocp exported the kubeconfig to %s, but the state now verifies as %s — kind-ocp will not return a kubeconfig from a tree it did not export into; point the state root back and re-run",
			path, verified)
	}
	return verified, nil
}

func joinNames(infos []ClusterInfo) string {
	names := make([]string, 0, len(infos))
	for _, i := range infos {
		names = append(names, i.Name)
	}
	return strings.Join(names, ", ")
}

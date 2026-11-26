// Package compliance is the Compliance Operator fitness proof: it
// installs a real Compliance Operator on one cluster through OLM — the
// same CatalogSource/OperatorGroup/Subscription flow OpenShift itself
// uses — and proves the cluster runs it to real scan results. It is
// the cluster-native port of the PoC's install_co OLM part,
// wait_profilebundles_valid, and the Central-side scan assertion
// (stackrox PR #23841): instead of asking Central whether every
// cluster produced results, it reads the cluster's own
// ComplianceCheckResults.
//
// The proof exists because the Compliance Operator is the first
// consumer these clusters were built for, and it is the strictest
// OpenShift-emulation test kind-ocp has: the operator's Deployment
// schedules through the master node label (node-labels layer), its
// scanner pods run as non-root without a UID and are admitted only
// through the SCC-UID webhook (scc-admission layer), its
// api-resource-collector reads the config.openshift.io objects the
// ocp4 OVAL checks evaluate (config-openshift, openshift-api-crds,
// and platform-objects layers), and it installs through OLM (olm
// layer). A cluster that passes this proof is fit for the workload
// the whole project exists to host; a cluster that is not fails at
// the step that is missing.
//
// This is a deployment proof, not a verify check, and the difference
// is the point: it installs an operator, creates scan workloads, and
// leaves them on the cluster — a Layer's Verify may mutate nothing it
// does not remove. It is therefore never reachable from the create
// path (nothing under cmd/, internal/cli, kindocp, layers, or
// internal/layers imports it; TestNothingImportsTheProofFromTheCreatePath
// fails the build if that changes), and its one consumer is the
// tagged integration test, which runs it per cluster against clusters
// the create path produced and deletes them after.
//
// Gotchas ported from the PoC (BRIEF §7.3) live with the code that
// needs them: the CatalogSource must sit in namespace `olm`, because
// OLM resolves Subscriptions only against its global catalog
// namespace; the compliance-operator-serving-cert secret must exist
// before the operator starts, or the operator exits in addMetrics on
// OpenShift platforms (a real cluster's service-ca injects the
// secret; here the proof creates it, with a Go-generated
// certificate, before the Subscription); and CSV resolution needs the
// fallback, because status.currentCSV may name a CSV the catalog
// never creates. Each wait runs under its own deadline — the PoC's
// reused-deadline bug (BRIEF §7.4) is not ported.
//
// Three more gotchas the committed catalog brought with it, all
// versioned and pinned behavior: the serving pod must satisfy the olm
// namespace's restricted PodSecurity (OLM's release labels it) and
// carry its olm.catalogSource label (the namespace is default-deny,
// and OLM's own ingress policy admits the catalog port only to pods
// so labeled); and the catalog must reach opm as one plain file inside
// an otherwise-empty directory, because a whole ConfigMap volume's
// kubelet-managed layout reads as duplicate catalog content to opm's
// walker.
//
// The Subscription deliberately carries no config.env: the committed
// catalog's CSV ignores the Subscription's PLATFORM and
// RELATED_IMAGE_PROFILE overrides (BRIEF §7.2), and the operator's
// own --platform flag defaults to OpenShift, so the default
// ProfileBundles (ocp4 and rhcos4) are created without any override.
// The catalog the proof installs from is committed in this repo
// (internal/verify/compliance/catalog), assembled from the pinned
// bundle manifests with every image digest-pinned, and served by the
// pinned image below — there is no catalog-image knob because there
// is no catalog image: bumping the Compliance Operator means
// regenerating the committed manifests (`make pin-compliance-catalog`)
// and re-running the proof.
//
// The proof is one-shot on a fresh cluster, and the serving-cert
// secret is its marker: Prove refuses to run where that secret
// already exists, because every wait behind it — the resolved CSV,
// the READY binding, the counted results — would be satisfied by the
// earlier run's stale state, and the proof would report a scan it
// never started. The proof's clusters are disposable by contract, so
// the refusal is the honest failure: delete and re-create the cluster
// to prove it again. The catalog is assembled and validated before
// that marker is created, so a proof that fails on bad local input
// leaves the cluster unmarked and a corrected retry possible.
package compliance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/guzalv/kind-ocp/internal/kube"
)

// DefaultCatalogServerImage is the digest-pinned image the proof's
// catalog server runs. It is the same upstream Compliance Operator
// catalog image the PoC pinned — the digest carries over verbatim —
// but its role narrowed: the pod that serves the proof's catalog uses
// the image's /bin/opm binary (and the grpc_health_probe its own
// readiness probes rely on) and ignores the upstream catalog baked
// into its /configs, because the catalog the proof serves is committed
// in this repo (see catalog.go). The single pin keeps the server
// deterministic: a floating tag here would float the only binary the
// proof's catalog resolution runs through. Bump it together with a
// regenerated catalog, after verifying the new image still ships a
// working opm.
const DefaultCatalogServerImage = "ghcr.io/complianceascode/compliance-operator-catalog@sha256:b5ad7b09f3fa12d99821b7768d6af54eba02e9328feb71c5aee7624401d5a3e8"

// The names of everything the proof installs. Fixed, not options:
// they are the proof's identity inside one cluster, and the operator
// namespace doubles as the scc-admission layer's default watched
// namespace, which is why the operator's scanner pods are admitted
// with an injected UID at all.
const (
	// operatorNamespace is the Compliance Operator's install namespace
	// (the upstream CSV's suggested namespace). The scc-admission
	// layer watches it by default; a cluster whose layer configuration
	// changed that default will fail the proof at the scan step — the
	// operator's runAsNonRoot pods then never run.
	operatorNamespace = "openshift-compliance"

	// catalogNamespace is OLM's global catalog namespace. The
	// CatalogSource must live here: OLM resolves Subscriptions only
	// against its global catalog namespace and the Subscription's own
	// namespace, and the proof's Subscription names this source.
	catalogNamespace = "olm"

	catalogSourceName  = "compliance-operator"
	operatorGroupName  = "compliance-operator"
	subscriptionName   = "compliance-operator-sub"
	operatorDeployment = "compliance-operator"

	// servingCertSecret is the secret the operator's addMetrics startup
	// requires to exist: OpenShift's service-ca injects it from the
	// annotation the operator's own metrics Service carries, and the
	// operator exits when it is absent (BRIEF §7.3). Kind has no
	// service-ca, so the proof creates it before the operator starts.
	servingCertSecret = "compliance-operator-serving-cert"

	// csvNamePrefix is the shape of every ClusterServiceVersion name
	// the compliance-operator package publishes
	// ("compliance-operator.v1.11.0-dev"); the CSV fallback selects
	// among these.
	csvNamePrefix = "compliance-operator."

	// The scan the proof runs: the ocp4-cis profile — a real platform
	// profile from the ocp4 ProfileBundle, whose OVAL checks read the
	// config.openshift.io objects the platform layers install, so the
	// results are real PASS/FAIL evaluations, not NOT-APPLICABLE. Node
	// profiles (rhcos4-*) cannot work on kind nodes (BRIEF §6) and are
	// not attempted.
	profileName = "ocp4-cis"

	scanSettingName = "kind-ocp-verify"
	bindingName     = "kind-ocp-verify-ocp4-cis"

	// defaultProfileBundles are the bundles the operator creates for
	// its default (OpenShift) platform; the proof waits for both to
	// parse their content image before it binds a scan to a profile
	// only the ocp4 bundle carries.
	defaultProfileBundles = "ocp4 rhcos4"
)

// profileBundleNames returns the ProfileBundles the proof waits on,
// in the order it waits on them.
func profileBundleNames() []string { return []string{"ocp4", "rhcos4"} }

// operatorsAPI is the API group/version the olm layer installs and the
// proof's CatalogSource, OperatorGroup, and Subscription live in; the
// preflight check asks for it, so a cluster without the olm layer
// fails in seconds instead of at the CSV wait's timeout.
const operatorsAPI = "operators.coreos.com/v1alpha1"

// Wait budgets. Fixed constants, not options: AC-3.1 requires bounded
// waits, no documented use case overrides a budget, and the
// integration test that needs short budgets drives prove through the
// budgets struct below instead. The values keep the PoC's headroom:
// its TIMEOUT_ROLLOUT (600s) covered cold image pulls on CI runners,
// and its results window (Central e2e) allowed fifteen minutes.
const (
	// preflightTimeout bounds the discovery read that checks OLM is
	// serving.
	preflightTimeout = 2 * time.Minute

	// catalogServerTimeout bounds the wait for the proof's catalog
	// server pod to become Ready: the pinned image's pull on a cold
	// node plus opm loading the committed catalog, with the headroom
	// the rollout budgets keep.
	catalogServerTimeout = 10 * time.Minute

	// csvResolveTimeout bounds the wait for the Subscription to resolve
	// an installable CSV (the catalog connection and OLM's resolution
	// both land here) and the wait for that CSV to reach phase
	// Succeeded — two waits, two fresh deadlines (BRIEF §7.4).
	csvResolveTimeout = 10 * time.Minute

	// csvSucceededTimeout bounds the second of those waits: the
	// install plan's applies and the operator image pull.
	csvSucceededTimeout = 10 * time.Minute

	// rolloutTimeout bounds the operator Deployment's rollout wait —
	// the operator image pull on a cold node.
	rolloutTimeout = 10 * time.Minute

	// profileBundleTimeout bounds each ProfileBundle's wait for
	// dataStreamStatus VALID: the content image pull and the
	// profileparser pod's parse, per bundle.
	profileBundleTimeout = 10 * time.Minute

	// bindingReadyTimeout bounds the wait for the ScanSettingBinding
	// to reach phase READY — the binding controller reading the
	// profile the ocp4 bundle created.
	bindingReadyTimeout = 2 * time.Minute

	// scanResultsTimeout bounds the wait for the scan's results. The
	// scanner pods pull the openscap and content images and evaluate
	// every ocp4-cis rule against the collected API resources; the
	// PoC's Central-side window was fifteen minutes at thirty-second
	// polls, and this keeps that headroom locally.
	scanResultsTimeout = 20 * time.Minute
)

// budgets carries every wait's bound, so the unit tests can drive the
// proof against a fake apiserver with budgets the test can out-live.
// Prove passes the production constants; prove is the test seam, the
// same pattern the layers' Verify implementations use.
type budgets struct {
	preflight, catalogServer, csvResolve, csvSucceeded, rollout, profileBundle, bindingReady, scanResults time.Duration
}

func defaultBudgets() budgets {
	return budgets{
		preflight:     preflightTimeout,
		catalogServer: catalogServerTimeout,
		csvResolve:    csvResolveTimeout,
		csvSucceeded:  csvSucceededTimeout,
		rollout:       rolloutTimeout,
		profileBundle: profileBundleTimeout,
		bindingReady:  bindingReadyTimeout,
		scanResults:   scanResultsTimeout,
	}
}

// Options configures one proof run.
type Options struct {
	// KubeconfigPath is the cluster-admin kubeconfig of the cluster to
	// prove. Required.
	KubeconfigPath string

	// Log receives the proof's progress lines. Nil means slog.Default.
	Log *slog.Logger
}

// Prove installs the Compliance Operator on the cluster behind
// opts.KubeconfigPath and proves it works end to end: the
// serving-cert secret before the operator starts, the proof's own
// catalog server (the committed, digest-pinned catalog served by the
// pinned opm image) in olm, the CatalogSource addressing it, the
// OperatorGroup and Subscription in the operator namespace, the
// resolved CSV reaching Succeeded (with the channel-head fallback),
// the operator Deployment rolled out, both default ProfileBundles
// VALID, and one ocp4-cis ScanSettingBinding producing at least one
// PASS and one FAIL ComplianceCheckResult — real OVAL evaluations,
// which is what proves the platform layers the checks read are in
// place. On success the operator, its objects, the catalog server,
// and the scan's results stay on the cluster (a deployment proof, not
// a check); the caller owns the cluster's lifecycle around it.
//
// Prove is one-shot: it refuses a cluster that already carries the
// proof's serving-cert secret, because every wait would be satisfied
// by that earlier run's stale CSV, binding, and results. Point it at
// a fresh cluster for every proof.
func Prove(ctx context.Context, opts Options) error {
	return prove(ctx, opts, defaultBudgets())
}

func prove(ctx context.Context, opts Options, b budgets) error {
	if opts.KubeconfigPath == "" {
		return errors.New("compliance proof: KubeconfigPath is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	// One load of the kubeconfig builds the one client set the whole
	// proof uses — the house rule the layers follow: applies, waits,
	// and reads all resolve exactly the cluster that one load
	// resolved, and the shared transport refuses redirects, so no
	// request of the proof is re-sent to an endpoint it did not
	// choose.
	cfg, err := kube.RESTConfig(opts.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("loading the kubeconfig: %w", err)
	}
	client, err := kube.NewNoRedirectClientFromConfig(cfg)
	if err != nil {
		return fmt.Errorf("building Kubernetes clients: %w", err)
	}

	// The one-shot guard, before anything else: a cluster that
	// already carries the serving-cert secret has carried this proof
	// before, and every wait below — the resolved CSV, the Succeeded
	// phase, the READY binding, the counted results — would be
	// satisfied by that run's stale state. The proof refuses instead
	// of reporting a scan it never started.
	if err := requireFreshCluster(ctx, client.Clientset); err != nil {
		return err
	}

	// Preflight: the proof installs through OLM, so a cluster without
	// the olm layer fails here, in seconds, with the reason — not at
	// the CSV wait's timeout with a mystery.
	preflightCtx, cancel := context.WithTimeout(ctx, b.preflight)
	served, err := kube.GroupVersionServed(preflightCtx, client.Discovery, operatorsAPI)
	cancel()
	if err != nil {
		return fmt.Errorf("checking for OLM: %w", err)
	}
	if !served {
		return fmt.Errorf("API %s is not served: the compliance proof installs through OLM — was the olm layer applied? (run create with the full profile first)", operatorsAPI)
	}

	// The catalog is assembled and validated here, before anything
	// lands on the cluster — in particular before the one-shot marker
	// the next step creates: a proof that fails on bad local input (a
	// committed manifest the assembly refuses) must not leave a
	// cluster that then refuses a corrected retry as a second proof.
	catalogObjects, err := catalogObjects()
	if err != nil {
		return fmt.Errorf("assembling the proof's catalog: %w", err)
	}

	// The serving-cert secret before anything can start the operator:
	// the operator's addMetrics exits when the secret is absent, and
	// the Subscription below is what starts the operator. The
	// namespaces come first so the secret's create has somewhere to
	// land.
	log.Info("creating the operator's serving-cert secret", "namespace", operatorNamespace)
	if err := client.Apply(ctx, fieldManager, namespaceObjects()); err != nil {
		return fmt.Errorf("applying the proof's namespaces: %w", err)
	}
	if err := createServingCert(ctx, client.Clientset); err != nil {
		return err
	}

	// The catalog before the install that resolves against it: the
	// committed catalog's ConfigMap, the pod that serves it, and the
	// Service the CatalogSource will address. The wait that follows
	// is the proof's own gate — Ready means the gRPC health probe
	// passed, so a catalog opm refuses to load, or an image the node
	// cannot pull, fails here in minutes with the pod's own state as
	// the reason, not at the CSV wait's timeout with a mystery.
	log.Info("staging the proof's catalog", "server", catalogServerName, "namespace", catalogNamespace)
	if err := client.Apply(ctx, fieldManager, catalogObjects); err != nil {
		return fmt.Errorf("applying the proof's catalog server: %w", err)
	}
	if err := waitForCatalogServer(ctx, client.Clientset, b.catalogServer); err != nil {
		return err
	}

	log.Info("installing the Compliance Operator through OLM", "catalog", catalogAddress(), "operatorNamespace", operatorNamespace)
	if err := client.Apply(ctx, fieldManager, installObjects()); err != nil {
		return fmt.Errorf("applying the Compliance Operator install objects: %w", err)
	}

	// The resolved CSV, then its Succeeded phase — two waits under two
	// fresh deadlines: the PoC reused one deadline across both and the
	// second wait could inherit the first's spent budget (BRIEF
	// §7.4).
	csv, err := waitForResolvedCSV(ctx, client.Dynamic, log, b.csvResolve)
	if err != nil {
		return err
	}
	if err := waitForCSVSucceeded(ctx, client.Dynamic, csv, b.csvSucceeded); err != nil {
		return err
	}
	if err := kube.WaitForDeploymentRolledOut(ctx, client.Clientset, operatorNamespace, operatorDeployment, b.rollout); err != nil {
		return fmt.Errorf("the Compliance Operator's Deployment did not roll out: %w", err)
	}
	log.Info("the Compliance Operator is running", "csv", csv)

	for _, pb := range profileBundleNames() {
		if err := waitForProfileBundle(ctx, client.Dynamic, pb, b.profileBundle); err != nil {
			return err
		}
	}
	log.Info("the default ProfileBundles are VALID", "bundles", defaultProfileBundles)

	log.Info("running one ocp4-cis scan", "profile", profileName)
	if err := client.Apply(ctx, fieldManager, scanObjects()); err != nil {
		return fmt.Errorf("applying the proof's scan objects: %w", err)
	}
	if err := waitForBindingReady(ctx, client.Dynamic, b.bindingReady); err != nil {
		return err
	}
	return waitForScanResults(ctx, client.Dynamic, log, b.scanResults)
}

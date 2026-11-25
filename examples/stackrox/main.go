// Command stackrox deploys StackRox Central and Sensor across a kind-ocp
// prefix: Central on cluster 1, Sensor on clusters 2..N, connected over a
// NodePort service on the shared kind network. It is the recipe-shaped
// counterpart of the Compliance Operator proof — where that proof shows a
// cluster is OpenShift-like enough for an operator, this one shows the
// multi-cluster topology those clusters exist to host, end to end: a
// workload pod on one cluster (Sensor) reaching a NodePort service on
// another (Central) through the network kind gives every cluster in the
// prefix.
//
// The example is deliberately not product functionality (BRIEF §6): kind-ocp
// creates clusters and applies layers; everything here deploys third-party
// software on top of them. It lives as a package main so nothing can import
// it — the create path can never reach it — and it drives kind-ocp through
// its public surface (config, kindocp) plus the standard Kubernetes Go
// clients, exactly as an outside consumer would. One thing it takes from
// inside the module: its credential workspace is verified with kind-ocp's
// internal filesystem boundary (internal/fscheck), the same policy the
// state tree walks — an outside consumer would keep its credentials under
// a caller-owned directory it verifies itself.
//
// Host requirements beyond kind-ocp's own (Docker): the helm CLI (v3 or
// newer) for chart installation, and network access to the StackRox chart
// repository and image registry. The charts are fetched once per run and
// verified against recorded SHA-256 digests; the images are the released
// public tags the chart itself references (quay.io/stackrox-io, appVersion
// 5.0.0), so a chart bump is an image bump — see the pins in charts.go.
//
// Secrets handling: the generated Central admin password and the cluster
// registration secret are written only to 0600 files (checked private
// before every read and write) under a verified 0700 work directory that
// sits beside kind-ocp's state root, passed to helm via values files
// (never argv, which other local users could read), and never logged.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/stackrox/kind-ocp/config"
)

// Phase timeouts. Each is a fixed budget like the Compliance Operator
// proof's, so a stalled phase fails with a clear error instead of hanging.
const (
	// helmTimeout bounds one helm upgrade --install, which waits for the
	// release's workloads to roll out. Cold Central image pulls land here.
	helmTimeout = 15 * time.Minute

	// centralReadyTimeout bounds the wait for Central's API to answer
	// after its deployment reported Available.
	centralReadyTimeout = 5 * time.Minute

	// clustersHealthyTimeout bounds the wait for every Sensor cluster to
	// register in Central and report HEALTHY.
	clustersHealthyTimeout = 10 * time.Minute
)

func main() {
	prefix := flag.String("prefix", "", "cluster name prefix (default kind-ocp)")
	count := flag.Int("count", 0, "number of clusters; Central on 1, Sensor on 2..N (default 2)")
	stateRoot := flag.String("state-root", "", "kind-ocp state root (default $XDG_STATE_HOME/kind-ocp)")
	flag.Parse()

	// The recipe controls the profile (full): the OpenShift layers are the
	// point of the exercise. Only the prefix, count, and state root are
	// exposed, and they resolve through kind-ocp's own precedence chain.
	cfg, err := resolveConfig(prefix, stateRoot, count)
	if err != nil {
		fail(err)
	}
	if cfg.Count < 2 {
		fail(fmt.Errorf("--count must be at least 2: the example puts Central on cluster 1 and Sensor on clusters 2..N (got %d)", cfg.Count))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := deploy(ctx, cfg, log); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "stackrox example: %v\n", err)
	os.Exit(1)
}

// deploy runs the whole recipe. Each phase's errors name the phase; the
// clusters are kind-ocp clusters and stay in place on failure, so the
// standard cleanup (kind-ocp delete) applies to every failure path.
func deploy(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	records, kubeconfigs, err := ensureClusters(ctx, cfg, log)
	if err != nil {
		return err
	}

	work, err := openWorkDir(cfg)
	if err != nil {
		return err
	}
	password, err := loadOrCreateAdminPassword(work)
	if err != nil {
		return err
	}

	helm, err := newHelmRunner()
	if err != nil {
		return err
	}

	centralChart, securedChart, err := fetchCharts(ctx, work)
	if err != nil {
		return err
	}
	log.Info("charts fetched and verified", "central", centralChart.version, "secured", securedChart.version)

	centralCluster := records[0].Name
	sensorClusters := make([]string, 0, len(records)-1)
	for _, rec := range records[1:] {
		sensorClusters = append(sensorClusters, rec.Name)
	}

	// Phase 1: Central on cluster 1, NodePort-exposed.
	centralKubeconfig := kubeconfigs[centralCluster]
	log.Info("deploying Central", "cluster", centralCluster)
	valuesPath, err := writeCentralValues(work, password)
	if err != nil {
		return err
	}
	if err := helm.upgradeInstall(ctx, os.Stderr, centralChart.dir, centralKubeconfig, valuesPath, helmTimeout); err != nil {
		return fmt.Errorf("deploying Central on %s: %w", centralCluster, err)
	}
	log.Info("Central deployed", "cluster", centralCluster)

	// Phase 2: the endpoint the Sensors use — the central cluster's node
	// address on the shared kind network plus Central's NodePort.
	centralCS, _, err := clientsetFor(centralKubeconfig)
	if err != nil {
		return err
	}
	endpoint, err := centralEndpoint(ctx, centralCS)
	if err != nil {
		return fmt.Errorf("discovering the Central endpoint on %s: %w", centralCluster, err)
	}
	log.Info("Central endpoint for Sensors", "endpoint", endpoint)

	// Phase 3: a cluster registration secret from Central's own API, via a
	// localhost port-forward (the Docker network's node addresses are not
	// reachable from the host on every platform; the Sensors, running in
	// containers on the shared network, use the endpoint above). Every
	// authenticated call runs under the forward's own context: it is
	// cancelled when the tunnel exits, so the admin password never travels
	// to a local port the forward no longer owns.
	pf, apiBase, apiCtx, err := startCentralPortForward(ctx, centralKubeconfig)
	if err != nil {
		return fmt.Errorf("port-forwarding to Central on %s: %w", centralCluster, err)
	}
	defer pf.close()

	api := newCentralAPI(apiBase, password)
	if err := waitForCentral(apiCtx, api, centralReadyTimeout, log); err != nil {
		return err
	}
	crsName := "kind-ocp-example-" + cfg.Prefix
	crsManifest, err := api.generateCRS(apiCtx, crsName, log)
	if err != nil {
		return fmt.Errorf("generating the cluster registration secret: %w", err)
	}
	log.Info("generated cluster registration secret", "name", crsName)

	// Phase 4: Sensor on every remaining cluster, each registered under its
	// cluster name against the shared endpoint.
	for _, name := range sensorClusters {
		log.Info("deploying Sensor", "cluster", name)
		valuesPath, err := writeSensorValues(work, name, endpoint, crsManifest)
		if err != nil {
			return err
		}
		if err := helm.upgradeInstall(ctx, os.Stderr, securedChart.dir, kubeconfigs[name], valuesPath, helmTimeout); err != nil {
			return fmt.Errorf("deploying Sensor on %s: %w", name, err)
		}
		log.Info("Sensor deployed", "cluster", name)
	}

	// Phase 5: the proof — Central reports every Sensor cluster registered
	// and HEALTHY. A HEALTHY Sensor is a workload pod on cluster N that
	// reached Central's NodePort on cluster 1 and stays connected.
	if err := waitForHealthyClusters(apiCtx, api, sensorClusters, clustersHealthyTimeout, log); err != nil {
		return err
	}

	printSummary(cfg, work, centralCluster, endpoint, centralKubeconfig, sensorClusters)
	return nil
}

func printSummary(cfg *config.Config, work, centralCluster, endpoint, centralKubeconfig string, sensorClusters []string) {
	fmt.Fprintf(os.Stderr, "\nStackRox is up: Central on %s at %s, Sensor on %s\n",
		centralCluster, endpoint, joinQuoted(sensorClusters))
	fmt.Fprintf(os.Stderr, "Admin password (not printed): %s\n", filepath.Join(work, adminPasswordFile))
	fmt.Fprintf(os.Stderr, "Central UI: kubectl --kubeconfig %s -n %s port-forward svc/%s 8443:443, then https://localhost:8443\n",
		shellQuote(centralKubeconfig), stackroxNamespace, centralServiceName)
	fmt.Fprintf(os.Stderr, "Cleanup: %s\n", deleteCommand(cfg))
}

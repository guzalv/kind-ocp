package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/stackrox/kind-ocp/config"
	"github.com/stackrox/kind-ocp/internal/fscheck"
)

// adminPasswordFile and the values file names, all under the work
// directory. The values files carry the admin password and the cluster
// registration secret; they are written 0600 and passed to helm with -f,
// never through argv.
const (
	adminPasswordFile = "admin-password"
	centralValuesFile = "values-central.yaml"
	sensorValuesFile  = "values-sensor-%s.yaml"
)

// helmRunner installs charts through the helm CLI. helm is the one host
// tool the example adds on top of kind-ocp's Docker requirement: installing
// these charts by hand-rendered manifests would duplicate the chart and
// drift, and the charts are the supported deployment surface.
type helmRunner struct {
	binary string
}

func newHelmRunner() (helmRunner, error) {
	binary, err := exec.LookPath("helm")
	if err != nil {
		return helmRunner{}, errors.New("the helm CLI is required (https://helm.sh): " + err.Error())
	}
	return helmRunner{binary: binary}, nil
}

// upgradeInstall runs `helm upgrade --install` for one release against one
// cluster's kubeconfig. It is idempotent: re-running the example converges
// the releases. Output goes to out (the run's stderr) so the evidence log
// carries helm's own progress; a failure wraps the command error with the
// tail of that output.
func (h helmRunner) upgradeInstall(ctx context.Context, out io.Writer, chartDir, kubeconfigPath, valuesPath string, timeout time.Duration) error {
	args := []string{
		"upgrade", "--install", releaseName(chartDir), chartDir,
		"--namespace", stackroxNamespace, "--create-namespace",
		"--kubeconfig", kubeconfigPath,
		"-f", valuesPath,
		"--wait", "--timeout", timeout.String(),
	}
	cmd := exec.CommandContext(ctx, h.binary, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// releaseName maps a chart directory to the release name the chart mandates
// (the charts refuse nonstandard release names). Both charts install into
// the fixed `stackrox` namespace under their own default release names.
func releaseName(chartDir string) string {
	if filepath.Base(chartDir) == "stackrox-secured-cluster-services" {
		return "stackrox-secured-cluster-services"
	}
	return "stackrox-central-services"
}

// loadOrCreateAdminPassword returns the Central admin password, generating
// and persisting one on first use. The password is reused across runs so a
// re-run's helm upgrade never invalidates the credentials the example (and
// the user, through the UI) already has. An existing password file is a
// credential: it must verify as a private, caller-owned regular file
// before it is read — a symlinked or shared file is refused, not followed
// (a substituted file could hand the run a password someone else knows).
func loadOrCreateAdminPassword(work string) (string, error) {
	path := filepath.Join(work, adminPasswordFile)
	if err := fscheck.CheckWriteTarget(path); err != nil {
		return "", fmt.Errorf("the admin password file: %w", err)
	}
	if b, err := os.ReadFile(path); err == nil {
		if password := strings.TrimSpace(string(b)); password != "" {
			return password, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating the admin password: %w", err)
	}
	password := base64.RawURLEncoding.EncodeToString(raw)
	if err := writeSecretFile(path, []byte(password+"\n")); err != nil {
		return "", err
	}
	return password, nil
}

// writeCentralValues renders the Central values document: NodePort
// exposure, no scanner, no OpenShift monitoring, no telemetry, and resource
// requests shrunk so Central, Central DB, and its sidecar deployments
// co-schedule on a single kind node (kind advertises the host's CPU; the
// chart defaults request 4 CPU for Central DB alone).
func writeCentralValues(work, adminPassword string) (string, error) {
	values := map[string]any{
		// Sensor deploys in vanilla-Kubernetes mode on purpose: the
		// OpenShift-ness of these clusters exists for the workloads'
		// API surface (the Compliance Operator's ProfileBundles and
		// the api-resource-collector), not for StackRox's own
		// deployment plumbing, which would try to build OpenShift
		// shapes (SCCs, Routes) these clusters do not serve.
		"env": map[string]any{"openshift": false},
		"imagePullSecrets": map[string]any{
			"allowNone": true,
		},
		"monitoring": map[string]any{
			"openshift": map[string]any{"enabled": false},
		},
		"central": map[string]any{
			"telemetry":     map[string]any{"enabled": false},
			"adminPassword": map[string]any{"value": adminPassword},
			"exposure": map[string]any{
				"nodePort": map[string]any{"enabled": true},
			},
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "500m", "memory": "1Gi"},
			},
			"db": map[string]any{
				"resources": map[string]any{
					"requests": map[string]any{"cpu": "500m", "memory": "1Gi"},
				},
			},
		},
		"centralWorker": map[string]any{
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "50m", "memory": "128Mi"},
			},
		},
		"configController": map[string]any{
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "50m", "memory": "128Mi"},
			},
		},
		"scannerV4": map[string]any{"disable": true},
	}
	return writeValuesFile(work, centralValuesFile, values)
}

// writeSensorValues renders one secured cluster's values document: the
// cluster's own name, the shared Central endpoint, the collector off (the
// compliance container stays — it is the piece that serves the Compliance
// Operator integration), admission control scaled to one replica, and the
// cluster registration secret that bootstraps the cluster's identity with
// Central.
func writeSensorValues(work, clusterName, endpoint, crsManifest string) (string, error) {
	values := map[string]any{
		"clusterName":     clusterName,
		"centralEndpoint": endpoint,
		"env":             map[string]any{"openshift": false},
		"imagePullSecrets": map[string]any{
			"allowNone": true,
		},
		"monitoring": map[string]any{
			"openshift": map[string]any{"enabled": false},
		},
		"scannerV4": map[string]any{"disable": true},
		"collector": map[string]any{
			"collectionMethod": "NO_COLLECTION",
		},
		"admissionControl": map[string]any{
			"replicas": 1,
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "50m", "memory": "128Mi"},
			},
		},
		"sensor": map[string]any{
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "200m", "memory": "512Mi"},
			},
		},
		// crs.file is the cluster registration secret manifest Central
		// generated, embedded verbatim; the chart creates the secret in
		// the cluster and the Sensor uses it to register.
		"crs": map[string]any{"file": crsManifest},
	}
	return writeValuesFile(work, fmt.Sprintf(sensorValuesFile, clusterName), values)
}

// writeValuesFile marshals a values document to a 0600 file under the work
// directory (values carry secrets: the admin password and the registration
// secret) and returns its path.
func writeValuesFile(work, name string, values map[string]any) (string, error) {
	b, err := yaml.Marshal(values)
	if err != nil {
		return "", err
	}
	path := filepath.Join(work, name)
	return path, writeSecretFile(path, b)
}

// writeSecretFile writes bytes to a 0600 file, refusing an existing
// file that is not a private, caller-owned regular file (a symlinked or
// shared file is refused, not written through — kind-ocp's own boundary
// policy, reused from internal/fscheck).
func writeSecretFile(path string, b []byte) error {
	if err := fscheck.CheckWriteTarget(path); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// shellQuote renders one argument for POSIX shells, the way kind-ocp's own
// printed commands do: wrap in single quotes, escaping embedded quotes.
// The example re-implements the five-line rule rather than importing
// kind-ocp's internal helper, because an example shows the public surface
// only.
func shellQuote(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// joinQuoted renders a short list for a summary line.
func joinQuoted(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, item := range items {
		quoted = append(quoted, shellQuote(item))
	}
	return strings.Join(quoted, " ")
}

// deleteCommand renders the cleanup command for the run's prefix and state
// root, quoted so a state root containing spaces survives a copy-paste. The
// empty --config pins it against an ambient KIND_OCP_CONFIG, exactly like
// the commands kind-ocp itself prints.
func deleteCommand(cfg *config.Config) string {
	return fmt.Sprintf("kind-ocp delete --config '' --prefix %s --state-root %s",
		shellQuote(cfg.Prefix), shellQuote(cfg.StateRoot))
}

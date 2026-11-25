package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
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

// valuesOverrides carries the optional user-supplied helm values files:
// one overlaid on the Central release's generated values, one on every
// Sensor release's. Both are empty when the flags were not passed.
type valuesOverrides struct {
	central string
	sensor  string
}

// checkValuesOverrides validates the optional values files before
// anything runs. They ride the example's privacy rule for files it
// reads: a helm values file can carry secrets (image pull credentials,
// proxy passwords), so a file that is not a private, caller-owned
// regular file is refused, not handed to helm — the same refusal the
// admin password gets. Each path must also be one helm loads as
// exactly that file (see checkHelmValuesPath). Validating up front
// fails a bad path before the clusters are touched; only the paths,
// never the contents, reach argv or the log.
func checkValuesOverrides(central, sensor string) (valuesOverrides, error) {
	var o valuesOverrides
	if central != "" {
		if err := fscheck.CheckPrivateFile(central); err != nil {
			return o, fmt.Errorf("--central-values: %w", err)
		}
		if err := checkHelmValuesPath(central); err != nil {
			return o, fmt.Errorf("--central-values: %w", err)
		}
		o.central = central
	}
	if sensor != "" {
		if err := fscheck.CheckPrivateFile(sensor); err != nil {
			return o, fmt.Errorf("--sensor-values: %w", err)
		}
		if err := checkHelmValuesPath(sensor); err != nil {
			return o, fmt.Errorf("--sensor-values: %w", err)
		}
		o.sensor = sensor
	}
	return o, nil
}

// checkHelmValuesPath refuses a values path that helm would not load
// as exactly the one checked file: helm parses each -f argument as a
// comma-separated list (verified against helm v4.3.1 — a path "a,b" is
// loaded as the two files a and b), so a pathname containing a comma
// makes helm load different, unchecked files or fail on fragments that
// do not exist. The example checks exactly one private file per
// values flag; refusing the ambiguous pathname is what keeps that
// check meaningful all the way into helm.
func checkHelmValuesPath(path string) error {
	if strings.Contains(path, ",") {
		return fmt.Errorf("the path %q contains a comma, which helm's -f treats as a separator between files — "+
			"move or rename the file (or a directory in its path) so the path carries none", path)
	}
	return nil
}

// upgradeInstall runs `helm upgrade --install` for one release against one
// cluster's kubeconfig. It is idempotent: re-running the example converges
// the releases.
//
// generatedValues is the values file the example wrote for this release;
// overrideValues is the optional user-supplied file (--central-values /
// --sensor-values) — helm merges the later file over the earlier one, so
// the override wins where the two disagree. Both are validated before
// anything runs (the generated files by construction, the override by
// checkValuesOverrides), and each is passed as its own -f so one flag
// always means one checked file. Only the paths cross into argv; file
// contents never reach the command line or the log.
//
// helm's own stdout and stderr are discarded, never forwarded: its
// diagnostics quote values-file content verbatim (a malformed YAML line
// is echoed back in the error), and the values files carry the admin
// password and the cluster registration secret. A failure reports the
// phase, the release, and the exit status, with the full command line —
// paths only — so the failure is reproducible by hand without the log
// carrying values content.
func (h helmRunner) upgradeInstall(ctx context.Context, chartDir, kubeconfigPath, generatedValues, overrideValues string, timeout time.Duration) error {
	args := []string{
		"upgrade", "--install", releaseName(chartDir), chartDir,
		"--namespace", stackroxNamespace, "--create-namespace",
		"--kubeconfig", kubeconfigPath,
	}
	values := []string{generatedValues}
	if overrideValues != "" {
		values = append(values, overrideValues)
	}
	for _, p := range values {
		if err := checkHelmValuesPath(p); err != nil {
			return err
		}
		args = append(args, "-f", p)
	}
	args = append(args, "--wait", "--timeout", timeout.String())
	// Stdout and stderr left unset: exec connects them to /dev/null.
	cmd := exec.CommandContext(ctx, h.binary, args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm %s: %w — helm's own output is suppressed (it can quote the values files, which carry secrets); re-run the command above by hand for its diagnostics",
			strings.Join(args, " "), err)
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

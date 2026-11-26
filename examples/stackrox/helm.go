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
// admin password gets. The parent directory is resolved through the
// same ancestor policy the workspace uses (see resolveValuesPath), and
// only the resolved absolute path helm is handed survives the check.
// A resolved path inside either tree the run rewrites is refused too
// (see refuseValuesOverrideInTree): the example's work directory
// (generated values files, extracted charts) and this prefix's kind-ocp
// state directory (the expected cluster count, cluster records,
// kubeconfigs — the tree `kind-ocp delete` removes). A values file
// carrying more than one name is refused as well (see
// checkIndependentValuesFile): a hard link's other name can be a file
// the run rewrites in place, and the check cannot see the other names.
// Validating up front fails a bad path before the clusters are
// touched; only the paths, never the contents, reach argv or the log.
func checkValuesOverrides(central, sensor, stateRoot, prefix string) (valuesOverrides, error) {
	workspace := workspaceBase(stateRoot)
	stateTree := stateTreePath(stateRoot, prefix)
	check := func(flag, path string) (string, error) {
		p, err := resolveValuesPath(path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", flag, err)
		}
		if err := refuseValuesOverrideInTree(p, workspace,
			"the example's work directory (generated values files, extracted charts)"); err != nil {
			return "", fmt.Errorf("%s: %w", flag, err)
		}
		if err := refuseValuesOverrideInTree(p, stateTree,
			"this prefix's kind-ocp state directory (the expected cluster count, cluster records, kubeconfigs; kind-ocp delete removes the whole tree)"); err != nil {
			return "", fmt.Errorf("%s: %w", flag, err)
		}
		return p, nil
	}
	var o valuesOverrides
	if central != "" {
		p, err := check("--central-values", central)
		if err != nil {
			return o, err
		}
		o.central = p
	}
	if sensor != "" {
		p, err := check("--sensor-values", sensor)
		if err != nil {
			return o, err
		}
		o.sensor = p
	}
	return o, nil
}

// resolveValuesPath turns one user-supplied values path into the
// absolute, checked path helm is handed.
//
// The parent directory is resolved with fscheck.ResolveDir — every
// component of the chain, aliases included, must pass kind-ocp's
// ancestor policy — because a private leaf check alone does not
// protect a path through a writable ancestor: whoever can write the
// parent can swap the entry between the check and the moment helm
// opens it. A permissive directory in the chain is therefore a
// refusal, before the clusters are touched.
//
// The leaf is then checked private at the resolved path, and that it
// is one independent file — a hard link's other name can be an
// in-place write target of this run (see checkIndependentValuesFile).
// Only that absolute path crosses into argv. A relative operand — or a
// bare "-", which helm v4.3.1 reads as stdin, silently ignoring a
// local file of that name — would make helm open whatever its own
// resolution picks, not the file that was checked; the absolute
// spelling of the same file loads normally (verified against helm
// v4.3.1).
//
// Last, the resolved path must be one helm loads as exactly that file:
// see checkHelmValuesPath.
func resolveValuesPath(path string) (string, error) {
	parent, err := fscheck.ResolveDir(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(parent, filepath.Base(path))
	if err := checkHelmValuesPath(resolved); err != nil {
		return "", err
	}
	if err := fscheck.CheckPrivateFile(resolved); err != nil {
		return "", err
	}
	if err := checkIndependentValuesFile(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// checkIndependentValuesFile refuses a values file whose inode carries
// more than one name (checkValuesLinkCount reads the count): the run
// rewrites several files in place — kind-ocp's cluster records and
// exported kubeconfigs among them — and a hard link makes the checked
// file and such a target one inode, so the write would replace the
// contents after the check accepted them. The check cannot enumerate a
// file's other names, so every multiply-linked values file is refused;
// an independent copy (cp, not ln) is the remedy the refusal names.
func checkIndependentValuesFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return checkValuesLinkCount(path, info)
}

// checkHelmValuesPath refuses a values path that helm would not load
// as exactly the one checked file: helm v4.3.1 parses each -f operand
// as a CSV record, so a comma splits it into several file names
// (verified: "a,b" loads the two files a and b), a double quote breaks
// the CSV field parsing, and a newline or carriage return ends the
// record early — leaving helm reading a different, shorter path than
// the one that was checked. The example checks exactly one private
// file per values flag; refusing the ambiguous pathname is what keeps
// that check meaningful all the way into helm.
func checkHelmValuesPath(path string) error {
	for _, bad := range ",\"\n\r" {
		if strings.ContainsRune(path, bad) {
			return fmt.Errorf("the path %q contains %q, which helm's -f parsing treats specially (it splits -f operands on commas and parses them as CSV fields) — "+
				"move or rename the file (or a directory in its path) so the path carries none", path, bad)
		}
	}
	return nil
}

// upgradeInstall runs `helm upgrade --install` for one release against one
// cluster's kubeconfig. It is idempotent: re-running the example converges
// the releases.
//
// generatedValues is the values file the example wrote for this release;
// overrideValues is the optional user-supplied file (--central-values /
// --sensor-values), already resolved to its absolute checked path by
// checkValuesOverrides — helm merges the later file over the earlier one,
// so the override wins where the two disagree. Both are validated before
// anything runs (the generated files by construction, the override by
// checkValuesOverrides), and each is passed as its own -f so one flag
// always means one checked file. Only the paths cross into argv; file
// contents never reach the command line or the log.
//
// helm's own stdout and stderr are discarded, never forwarded: its
// diagnostics quote values-file content verbatim (a malformed YAML line
// is echoed back in the error), and the values files carry the admin
// password and the cluster registration secret. A failure reports the
// exit status and the full command line — every argument and the helm
// binary itself shell-quoted, so copying it into a shell reproduces
// the run even across paths containing spaces or shell
// metacharacters. The warning stands both ways: helm's output is
// suppressed here because it can quote the values files, so re-running
// the command by hand prints that content to your terminal only.
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
		return fmt.Errorf("helm failed: %w — its output is suppressed here (it can quote the values files, which carry secrets); for helm's own diagnostics re-run by hand: %s %s",
			err, shellQuote(h.binary), joinQuoted(args))
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
// policy, reused from internal/fscheck). What passes the preflight is
// then replaced, never written through: the bytes go to a private
// staging file os.CreateTemp names uniquely in the same directory, and
// a rename publishes them over the target. A rename replaces the
// directory entry itself, so nothing is ever written through the inode
// at path — a symlink parked at the target between the preflight and
// the write is replaced, not followed, and a hard link to the target
// keeps its bytes when the run regenerates the file. The override
// flags refuse a multiply-linked values file at startup (see
// checkIndependentValuesFile), so the links this protects are the ones
// no flag ever names — a user file linked to a generated one for
// inspection survives the run that regenerates it.
func writeSecretFile(path string, b []byte) error {
	if err := fscheck.CheckWriteTarget(path); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	// Unconditional and deferred: after a successful rename the staging
	// name is already gone and removing it is a no-op, while any
	// failure — write, close, or the rename itself — never leaves this
	// writer's staging file behind.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
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

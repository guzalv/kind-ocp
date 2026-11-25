package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestWriteCentralValues(t *testing.T) {
	work := t.TempDir()
	path, err := writeCentralValues(work, "s3cret-password")
	if err != nil {
		t.Fatalf("writeCentralValues = %v", err)
	}
	if path != filepath.Join(work, centralValuesFile) {
		t.Errorf("writeCentralValues returned %q, want the file's full path (helm -f needs it)", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the values file mode = %v err %v, want 0600 (it carries the admin password)", info, err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(b, &values); err != nil {
		t.Fatalf("the central values are not valid YAML: %v", err)
	}
	central := values["central"].(map[string]any)
	if got := central["adminPassword"].(map[string]any)["value"]; got != "s3cret-password" {
		t.Errorf("central.adminPassword.value = %v", got)
	}
	exposure := central["exposure"].(map[string]any)["nodePort"].(map[string]any)
	if exposure["enabled"] != true {
		t.Error("central.exposure.nodePort.enabled is not set: Sensors would have no endpoint")
	}
	if central["telemetry"].(map[string]any)["enabled"] != false {
		t.Error("central.telemetry.enabled is not false: the example must not phone home")
	}
	if values["scannerV4"].(map[string]any)["disable"] != true {
		t.Error("scannerV4 is not disabled")
	}
	if values["env"].(map[string]any)["openshift"] != false {
		t.Error("env.openshift is not pinned to false")
	}
	requests := central["resources"].(map[string]any)["requests"].(map[string]any)
	if requests["cpu"] != "500m" || requests["memory"] != "1Gi" {
		t.Errorf("central resource requests = %v, want the shrunk values a kind node can schedule", requests)
	}
	dbRequests := central["db"].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)
	if dbRequests["cpu"] != "500m" || dbRequests["memory"] != "1Gi" {
		t.Errorf("central.db resource requests = %v, want the shrunk values", dbRequests)
	}
}

func TestWriteSensorValues(t *testing.T) {
	work := t.TempDir()
	path, err := writeSensorValues(work, "kind-ocp-2", "https://172.18.0.2:31443", testCRSManifest)
	if err != nil {
		t.Fatalf("writeSensorValues = %v", err)
	}
	if path != filepath.Join(work, "values-sensor-kind-ocp-2.yaml") {
		t.Errorf("writeSensorValues returned %q, want the per-cluster file's full path", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the values file mode = %v err %v, want 0600 (it carries the registration secret)", info, err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(b, &values); err != nil {
		t.Fatalf("the sensor values are not valid YAML: %v", err)
	}
	if values["clusterName"] != "kind-ocp-2" {
		t.Errorf("clusterName = %v", values["clusterName"])
	}
	if values["centralEndpoint"] != "https://172.18.0.2:31443" {
		t.Errorf("centralEndpoint = %v", values["centralEndpoint"])
	}
	if values["collector"].(map[string]any)["collectionMethod"] != "NO_COLLECTION" {
		t.Error("the collector is not disabled (collectionMethod NO_COLLECTION)")
	}
	if values["admissionControl"].(map[string]any)["replicas"] != 1 {
		t.Error("admissionControl.replicas is not 1")
	}
	// The registration secret rides under crs.file, byte-for-byte: the
	// chart validates and creates the secret from exactly this string.
	if values["crs"].(map[string]any)["file"] != testCRSManifest {
		t.Errorf("crs.file does not carry the manifest verbatim:\n%v", values["crs"])
	}
}

func TestLoadOrCreateAdminPasswordCreatesThenReuses(t *testing.T) {
	work := t.TempDir()
	first, err := loadOrCreateAdminPassword(work)
	if err != nil {
		t.Fatalf("first loadOrCreateAdminPassword = %v", err)
	}
	if len(first) < 20 {
		t.Errorf("the generated password is too short to be one (%d chars)", len(first))
	}
	if info, err := os.Stat(filepath.Join(work, adminPasswordFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the password file mode = %v err %v, want 0600", info, err)
	}
	second, err := loadOrCreateAdminPassword(work)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("the admin password was not reused across runs; a re-run would invalidate the credentials")
	}
}

// An existing credential file that is not private is refused before it
// is read: a shared password file could have been written — or read — by
// another local user.
func TestLoadOrCreateAdminPasswordRefusesAPermissiveExistingFile(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, adminPasswordFile)
	if err := os.WriteFile(path, []byte("known-password"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateAdminPassword(work); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("loadOrCreateAdminPassword = %v, want a mode rejection", err)
	}
	// The refusal must not have replaced the file's contents either.
	if b, err := os.ReadFile(path); err != nil || string(b) != "known-password" {
		t.Errorf("the refused file was rewritten: (%q, %v)", b, err)
	}
}

// A symlinked credential file is refused, not followed — even when the
// target is a private file this user owns: whatever left the link could
// repoint it, and the run would write the password through it (or read
// a password someone else chose).
func TestLoadOrCreateAdminPasswordRefusesASymlinkedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("chosen-by-someone-else"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(work, adminPasswordFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateAdminPassword(work); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("loadOrCreateAdminPassword = %v, want a symlink rejection", err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "chosen-by-someone-else" {
		t.Errorf("the symlink target was written through: (%q, %v)", b, err)
	}
}

func TestWriteSecretFileRefusesAPermissiveExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(path, []byte("new")); err == nil {
		t.Fatal("writeSecretFile overwrote a world-readable file")
	}
}

func TestWriteSecretFileRefusesASymlinkedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "link")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(path, []byte("new")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("writeSecretFile = %v, want a symlink rejection", err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "old" {
		t.Errorf("the symlink target was written through: (%q, %v)", b, err)
	}
}

// The values overrides ride the same privacy rule as the credentials:
// a values file can carry secrets, so it must be a private,
// caller-owned regular file before helm is pointed at it, and a file
// that is not is refused with the flag that named it.
func TestCheckValuesOverrides(t *testing.T) {
	t.Run("no flags, no overrides", func(t *testing.T) {
		o, err := checkValuesOverrides("", "")
		if err != nil || o.central != "" || o.sensor != "" {
			t.Fatalf("checkValuesOverrides() = (%+v, %v), want empty overrides and no error", o, err)
		}
	})
	t.Run("private files pass", func(t *testing.T) {
		dir := t.TempDir()
		central := filepath.Join(dir, "central.yaml")
		sensor := filepath.Join(dir, "sensor.yaml")
		for _, p := range []string{central, sensor} {
			if err := os.WriteFile(p, []byte("image: {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		o, err := checkValuesOverrides(central, sensor)
		if err != nil {
			t.Fatalf("checkValuesOverrides = %v, want acceptance", err)
		}
		if o.central != central || o.sensor != sensor {
			t.Errorf("overrides = %+v, want the given paths", o)
		}
	})
	t.Run("missing file is refused", func(t *testing.T) {
		if _, err := checkValuesOverrides(filepath.Join(t.TempDir(), "absent.yaml"), ""); err == nil {
			t.Fatal("a missing values file must be refused")
		}
	})
	t.Run("permissive file is refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(path, []byte("image: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := checkValuesOverrides("", path)
		if err == nil || !strings.Contains(err.Error(), "--sensor-values") {
			t.Fatalf("checkValuesOverrides = %v, want a refusal naming --sensor-values", err)
		}
	})
	t.Run("symlinked file is refused", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("image: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "values.yaml")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := checkValuesOverrides(link, ""); err == nil {
			t.Fatal("a symlinked values file must be refused, not followed")
		}
		if b, err := os.ReadFile(target); err != nil || string(b) != "image: {}\n" {
			t.Errorf("the symlink target changed: (%q, %v)", b, err)
		}
	})
}

// helmRunner hands helm one -f per values file, in order: the generated
// values first, the user's override after — helm merges later files over
// earlier ones, so the override wins where the two disagree. The test
// runs a stub helm that records its argv, so the assertion is about the
// real command line, not a rendered string.
func TestUpgradeInstallOrdersValuesFiles(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX sh available")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "argv")
	stub := filepath.Join(dir, "helm")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(argsFile) + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	generated := filepath.Join(dir, "values-sensor-kind-ocp-2.yaml")
	override := filepath.Join(dir, "my-sensor.yaml")
	runner := helmRunner{binary: stub}
	err := runner.upgradeInstall(context.Background(), io.Discard,
		"/work/charts/stackrox-secured-cluster-services", "/state/clusters/kind-ocp-2/kubeconfig",
		[]string{generated, override}, time.Minute)
	if err != nil {
		t.Fatalf("upgradeInstall = %v", err)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	want := []string{
		"upgrade", "--install", "stackrox-secured-cluster-services", "/work/charts/stackrox-secured-cluster-services",
		"--namespace", "stackrox", "--create-namespace",
		"--kubeconfig", "/state/clusters/kind-ocp-2/kubeconfig",
		"-f", generated, "-f", override,
		"--wait", "--timeout", "1m0s",
	}
	if !slices.Equal(args, want) {
		t.Errorf("helm argv = %q, want %q (override after the generated values)", args, want)
	}
}

func TestReleaseNameFollowsTheChart(t *testing.T) {
	if got := releaseName("/work/charts/stackrox-central-services"); got != "stackrox-central-services" {
		t.Errorf("releaseName(central chart) = %q", got)
	}
	if got := releaseName("/work/charts/stackrox-secured-cluster-services"); got != "stackrox-secured-cluster-services" {
		t.Errorf("releaseName(secured chart) = %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":       "'plain'",
		"with space":  "'with space'",
		"it's quoted": "'it'\\''s quoted'",
		"/safe/path":  "'/safe/path'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeleteCommandQuotesItsArguments(t *testing.T) {
	cfg := mustConfig(t, "team-dev", "/state root/with 'quotes'")
	cmd := deleteCommand(cfg)
	if !strings.Contains(cmd, "--prefix 'team-dev'") {
		t.Errorf("the delete command does not quote the prefix: %s", cmd)
	}
	if !strings.Contains(cmd, "--state-root '/state root/with '\\''quotes'\\''") {
		t.Errorf("the delete command does not quote the state root: %s", cmd)
	}
	if !strings.Contains(cmd, "--config ''") {
		t.Errorf("the delete command does not pin an empty --config: %s", cmd)
	}
}

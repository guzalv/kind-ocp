package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

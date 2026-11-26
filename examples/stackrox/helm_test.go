package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/guzalv/kind-ocp/config"
	"github.com/guzalv/kind-ocp/internal/fscheck"
	"gopkg.in/yaml.v3"
)

// mustStateRoot returns an existing state root in its own temp
// directory, so the work directory checkValuesOverrides derives from it
// sits in a different tree than any override file the test places.
func mustStateRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "kind-ocp")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("creating the state root: %v", err)
	}
	return root
}

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

// A generated values file is replaced, never written through: a user
// file hard-linked to it (one inode, two names — the shape a previous
// run's generated file linked from the user's own tree has) keeps its
// bytes, and the generated path takes the fresh document on a new
// inode. The in-place write this regression pins against replaced the
// user's custom image through the shared inode, and helm — handed the
// generated path and the user's file as two -f operands — read the
// generated content twice.
func TestWriteValuesFilesPreserveAHardLinkedFile(t *testing.T) {
	work := t.TempDir()
	user := filepath.Join(t.TempDir(), "my-sensor.yaml")
	original := []byte("image:\n  main:\n    fullRef: my-registry.example.com/stackrox/main:my-branch\n")
	if err := os.WriteFile(user, original, 0o600); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(work, fmt.Sprintf(sensorValuesFile, "kind-ocp-2"))
	if err := os.Link(user, generated); err != nil {
		t.Skipf("the volume does not support hard links (%v); the shared-inode shape cannot be built here", err)
	}

	path, err := writeSensorValues(work, "kind-ocp-2", "https://172.18.0.2:31443", testCRSManifest)
	if err != nil {
		t.Fatalf("writeSensorValues = %v", err)
	}
	if path != generated {
		t.Fatalf("writeSensorValues returned %q, want the generated path %q", path, generated)
	}
	// The original bytes must survive the run.
	if b, err := os.ReadFile(user); err != nil || !bytes.Equal(b, original) {
		t.Fatalf("the hard-linked user file was rewritten: (%q, %v), want the original bytes untouched", b, err)
	}
	fresh, err := os.ReadFile(generated)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(fresh, original) {
		t.Fatal("the generated values file still holds the user's bytes — the run did not regenerate it")
	}
	var values map[string]any
	if err := yaml.Unmarshal(fresh, &values); err != nil || values["clusterName"] != "kind-ocp-2" {
		t.Fatalf("the generated values file holds %q, want the regenerated document", fresh)
	}
	userInfo, err := os.Stat(user)
	if err != nil {
		t.Fatal(err)
	}
	generatedInfo, err := os.Stat(generated)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(userInfo, generatedInfo) {
		t.Fatal("the generated path still names the user's inode — helm would read one file twice")
	}
	// The staging file never survives the write.
	if entries, err := os.ReadDir(work); err != nil || len(entries) != 1 {
		t.Errorf("the work directory holds %v (%v) after the write, want the values file alone", entries, err)
	}
}

// The values overrides ride the same privacy rule as the credentials:
// a values file can carry secrets, so it must be a private,
// caller-owned regular file before helm is pointed at it, and a file
// that is not is refused with the flag that named it.
func TestCheckValuesOverrides(t *testing.T) {
	t.Run("no flags, no overrides", func(t *testing.T) {
		o, err := checkValuesOverrides("", "", mustStateRoot(t), "p")
		if err != nil || o.central != "" || o.sensor != "" {
			t.Fatalf("checkValuesOverrides() = (%+v, %v), want empty overrides and no error", o, err)
		}
	})
	t.Run("private files pass and come back as their resolved absolute paths", func(t *testing.T) {
		dir := t.TempDir()
		central := filepath.Join(dir, "central.yaml")
		sensor := filepath.Join(dir, "sensor.yaml")
		for _, p := range []string{central, sensor} {
			if err := os.WriteFile(p, []byte("image: {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		o, err := checkValuesOverrides(central, sensor, mustStateRoot(t), "p")
		if err != nil {
			t.Fatalf("checkValuesOverrides = %v, want acceptance", err)
		}
		resolved, err := fscheck.ResolveDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if o.central != filepath.Join(resolved, "central.yaml") || o.sensor != filepath.Join(resolved, "sensor.yaml") {
			t.Errorf("overrides = %+v, want the resolved paths under %s", o, resolved)
		}
	})
	t.Run("a relative path — including a file named -, which helm reads as stdin — resolves to its absolute checked path", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		dash := filepath.Join(dir, "-")
		if err := os.WriteFile(dash, []byte("image: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		o, err := checkValuesOverrides("-", "", mustStateRoot(t), "p")
		if err != nil {
			t.Fatalf("checkValuesOverrides(\"-\") = %v, want the dash file resolved, not refused or passed through", err)
		}
		resolved, err := fscheck.ResolveDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(resolved, "-")
		if o.central != want {
			t.Errorf("override = %q, want the resolved absolute path %q (a bare \"-\" would make helm read stdin)", o.central, want)
		}
	})
	t.Run("a values file reached through a writable ancestor is refused", func(t *testing.T) {
		dir := t.TempDir()
		writable := filepath.Join(dir, "writable")
		if err := os.Mkdir(writable, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(writable, "values.yaml")
		if err := os.WriteFile(path, []byte("image: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// The leaf stays private; only the parent becomes permissive —
		// whoever can write the parent can swap the entry between the
		// check and the moment helm opens it.
		if err := os.Chmod(writable, 0o0777); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(writable, 0o700) }()
		_, err := checkValuesOverrides(path, "", mustStateRoot(t), "p")
		if err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("checkValuesOverrides through a writable ancestor = %v, want a refusal naming the writable directory", err)
		}
	})
	t.Run("missing file is refused", func(t *testing.T) {
		if _, err := checkValuesOverrides(filepath.Join(t.TempDir(), "absent.yaml"), "", mustStateRoot(t), "p"); err == nil {
			t.Fatal("a missing values file must be refused")
		}
	})
	t.Run("permissive file is refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(path, []byte("image: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := checkValuesOverrides("", path, mustStateRoot(t), "p")
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
		if _, err := checkValuesOverrides(link, "", mustStateRoot(t), "p"); err == nil {
			t.Fatal("a symlinked values file must be refused, not followed")
		}
		if b, err := os.ReadFile(target); err != nil || string(b) != "image: {}\n" {
			t.Errorf("the symlink target changed: (%q, %v)", b, err)
		}
	})
	t.Run("path containing a comma is refused", func(t *testing.T) {
		dir := t.TempDir()
		// A directory whose name carries the comma, so the file itself
		// is a perfectly private regular file and only the path is
		// ambiguous — exactly the shape helm would split.
		amb := filepath.Join(dir, "weird,name")
		if err := os.Mkdir(amb, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(amb, "values.yaml")
		if err := os.WriteFile(path, []byte("image: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := checkValuesOverrides(path, "", mustStateRoot(t), "p")
		if err == nil || !strings.Contains(err.Error(), "comma") {
			t.Fatalf("checkValuesOverrides = %v, want a comma refusal naming the separator", err)
		}
		if !strings.Contains(err.Error(), "--central-values") {
			t.Fatalf("checkValuesOverrides = %v, want a refusal naming --central-values", err)
		}
	})
	t.Run("paths containing a quote a newline or a carriage return are refused", func(t *testing.T) {
		dir := t.TempDir()
		for _, tc := range []struct {
			name string
			leaf string
			bad  rune
		}{
			// helm v4.3.1 parses each -f operand as a CSV record: a
			// double quote breaks the field parsing, and a newline or
			// carriage return ends the record early (verified live:
			// the quote fails with "bare \" in non-quoted-field", the
			// newline leaves helm reading only the first line, the
			// carriage return fails URL parsing).
			{"quote", `qu"ote.yaml`, '"'},
			{"newline", "ba\nd.yaml", '\n'},
			{"carriage return", "cr\rd.yaml", '\r'},
		} {
			t.Run(tc.name, func(t *testing.T) {
				path := filepath.Join(dir, tc.leaf)
				if err := os.WriteFile(path, []byte("image: {}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				_, err := checkValuesOverrides("", path, mustStateRoot(t), "p")
				if err == nil || !strings.Contains(err.Error(), strconv.QuoteRune(tc.bad)) {
					t.Fatalf("checkValuesOverrides(%q) = %v, want a refusal naming %q", tc.leaf, err, tc.bad)
				}
				if !strings.Contains(err.Error(), "--sensor-values") {
					t.Fatalf("checkValuesOverrides = %v, want a refusal naming --sensor-values", err)
				}
			})
		}
	})
}

// An override may not point into the example's own work directory: the
// run rewrites that tree — the generated values files on every run,
// each chart's extraction directory on every fetch — so an override
// there would be overwritten or removed by the run that accepted it,
// and helm would receive the generated file and the override as two
// operands naming one file. Every shape here must be refused at
// startup, with the refused input left exactly as it was and the work
// directory holding nothing the run would have written.
func TestCheckValuesOverridesRefusesTheWorkDirectory(t *testing.T) {
	assertRefused := func(t *testing.T, flag string, cfg *config.Config, override string, content []byte) {
		var central, sensor string
		if flag == "--central-values" {
			central = override
		} else {
			sensor = override
		}
		_, err := checkValuesOverrides(central, sensor, cfg.StateRoot, cfg.Prefix)
		if err == nil || !strings.Contains(err.Error(), "work directory") {
			t.Fatalf("checkValuesOverrides inside the work directory = %v, want a refusal naming the work directory", err)
		}
		if !strings.Contains(err.Error(), flag) {
			t.Fatalf("the refusal must name the flag: %v", err)
		}
		// The refused input stays unchanged, and the refusal itself
		// writes nothing: the work directory holds the user's file and
		// nothing else — no generated values, no admin password, no
		// charts.
		if b, err := os.ReadFile(override); err != nil || !bytes.Equal(b, content) {
			t.Errorf("the refused override changed: (%q, %v), want the original bytes untouched", b, err)
		}
		if entries, err := os.ReadDir(filepath.Dir(override)); err != nil || len(entries) != 1 {
			t.Errorf("the work directory holds %v (%v) after the refusal, want only the user's file", entries, err)
		}
	}
	// The stale-values shape: the generated Sensor values name a previous
	// run left behind, carrying the user's own image edits.
	userValues := []byte("image:\n  main:\n    fullRef: my-registry.example.com/stackrox/main:my-branch\n")
	setup := func(t *testing.T, prefix string) (*config.Config, string) {
		base := t.TempDir()
		cfg := mustWorkConfig(t, base, prefix)
		work, err := openWorkDir(cfg)
		if err != nil {
			t.Fatal(err)
		}
		override := filepath.Join(work, fmt.Sprintf(sensorValuesFile, prefix+"-2"))
		if err := os.WriteFile(override, userValues, 0o600); err != nil {
			t.Fatal(err)
		}
		return cfg, override
	}

	t.Run("the generated values path inside the workspace", func(t *testing.T) {
		cfg, override := setup(t, "refuse")
		assertRefused(t, "--sensor-values", cfg, override, userValues)
	})
	t.Run("the generated central values path, named by --central-values", func(t *testing.T) {
		// The central flag rides the same refusal as the sensor one;
		// this pins it so the two cannot drift apart.
		base := t.TempDir()
		cfg := mustWorkConfig(t, base, "refuse-central")
		work, err := openWorkDir(cfg)
		if err != nil {
			t.Fatal(err)
		}
		override := filepath.Join(work, centralValuesFile)
		if err := os.WriteFile(override, userValues, 0o600); err != nil {
			t.Fatal(err)
		}
		assertRefused(t, "--central-values", cfg, override, userValues)
	})
	t.Run("under the extracted charts", func(t *testing.T) {
		// fetchCharts clears each chart's extraction directory before
		// extracting, so an override there would be removed outright.
		base := t.TempDir()
		cfg := mustWorkConfig(t, base, "charts")
		work, err := openWorkDir(cfg)
		if err != nil {
			t.Fatal(err)
		}
		charts := filepath.Join(work, "charts", "stackrox-central-services")
		if err := os.MkdirAll(charts, 0o700); err != nil {
			t.Fatal(err)
		}
		override := filepath.Join(charts, "my-central.yaml")
		if err := os.WriteFile(override, userValues, 0o600); err != nil {
			t.Fatal(err)
		}
		assertRefused(t, "--sensor-values", cfg, override, userValues)
	})
	t.Run("after kind-ocp delete removed the state root but the workspace survived", func(t *testing.T) {
		// The work directory outlives the state tree (kind-ocp delete
		// removes its own tree only). With the state root gone, the
		// derivation falls back to the state root's absolute spelling,
		// and the identity comparison follows the alias spellings
		// between it and the resolved workspace the previous run
		// created (macOS temp dirs live under the /var symlink).
		cfg, override := setup(t, "survive")
		if err := os.RemoveAll(cfg.StateRoot); err != nil {
			t.Fatal(err)
		}
		assertRefused(t, "--sensor-values", cfg, override, userValues)
	})
	t.Run("a case-colliding spelling of the workspace", func(t *testing.T) {
		base := t.TempDir()
		if !caseInsensitiveVolume(t, base) {
			t.Skip("the volume resolves names case-sensitively; a case-colliding spelling cannot denote one directory here")
		}
		cfg := mustWorkConfig(t, base, "casetest")
		work, err := openWorkDir(cfg)
		if err != nil {
			t.Fatal(err)
		}
		// Spell the workspace's own base directory in the other case:
		// on this volume both spellings denote the one directory while
		// remaining two strings — exactly the shape a string prefix
		// comparison would pass.
		spelled := filepath.Join(
			filepath.Dir(filepath.Dir(work)),
			strings.ToUpper(filepath.Base(filepath.Dir(work))),
			filepath.Base(work), "my-sensor.yaml")
		if err := os.WriteFile(spelled, userValues, 0o600); err != nil {
			t.Fatal(err)
		}
		assertRefused(t, "--sensor-values", cfg, spelled, userValues)
	})
	t.Run("a workspace candidate that cannot be checked fails closed", func(t *testing.T) {
		// A stat failure on the candidate that is not "does not exist"
		// must be a startup error naming the path, never a silent
		// pass: the check cannot see the tree, so it cannot say the
		// override is outside it. The state root is spelled under a
		// regular file, so the workspace candidate's stat fails with
		// ENOTDIR — a deterministic failure for every caller,
		// including a privileged one a mode-000 directory would not
		// stop.
		dir := t.TempDir()
		plain := filepath.Join(dir, "plainfile")
		if err := os.WriteFile(plain, []byte("not a directory\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir() // readable: only the candidate is blind
		path := filepath.Join(outside, "my-sensor.yaml")
		if err := os.WriteFile(path, userValues, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := checkValuesOverrides("", path, filepath.Join(plain, "kind-ocp"), "p")
		if err == nil || !strings.Contains(err.Error(), "work directory") {
			t.Fatalf("checkValuesOverrides with an uncheckable work directory = %v, want a startup error naming it", err)
		}
		if !strings.Contains(err.Error(), "--sensor-values") {
			t.Fatalf("the refusal must name the flag: %v", err)
		}
	})
	t.Run("a file beside the work directory is not refused", func(t *testing.T) {
		// The negative control: the workspace's own parent holds the
		// state root and the workspace side by side, and a user file
		// there is outside both — refusing it would be a false
		// positive.
		base := t.TempDir()
		cfg := mustWorkConfig(t, base, "outside")
		if _, err := openWorkDir(cfg); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(base, "my-sensor.yaml")
		if err := os.WriteFile(path, userValues, 0o600); err != nil {
			t.Fatal(err)
		}
		o, err := checkValuesOverrides("", path, cfg.StateRoot, cfg.Prefix)
		if err != nil || o.sensor == "" {
			t.Fatalf("checkValuesOverrides beside the work directory = (%+v, %v), want acceptance", o, err)
		}
	})
}

// An override may not point into this prefix's kind-ocp state
// directory either: the run rewrites that tree (the expected-cluster
// count on every create, the cluster records, the exported
// kubeconfigs), and `kind-ocp delete` removes it outright. Every shape
// here must be refused at startup with the file left exactly as it
// was.
func TestCheckValuesOverridesRefusesTheStateDirectory(t *testing.T) {
	// A marked per-prefix state tree, the shape a real create leaves
	// behind — the walk SetExpectedClusters and every record write go
	// through accepts it.
	newStateTree := func(t *testing.T, prefix string) (*config.Config, string) {
		base := t.TempDir()
		cfg := mustWorkConfig(t, base, prefix)
		tree := filepath.Join(cfg.StateRoot, cfg.Prefix)
		if err := os.MkdirAll(tree, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tree, "kind-ocp-state.json"), []byte(`{"tool":"kind-ocp"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return cfg, tree
	}
	// The parked-file shape: valid custom image values parked at the
	// expected-clusters path — the file SetExpectedClusters replaces
	// with the count before the first cluster is provisioned.
	userValues := []byte("image:\n  main:\n    fullRef: my-registry.example.com/stackrox/main:my-branch\n")

	t.Run("the expected-clusters path inside a marked state tree", func(t *testing.T) {
		cfg, tree := newStateTree(t, "refuse-state")
		override := filepath.Join(tree, "expected-clusters")
		if err := os.WriteFile(override, userValues, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := checkValuesOverrides("", override, cfg.StateRoot, cfg.Prefix)
		if err == nil || !strings.Contains(err.Error(), "state directory") {
			t.Fatalf("checkValuesOverrides inside the state directory = %v, want a refusal naming the state directory", err)
		}
		if !strings.Contains(err.Error(), "--sensor-values") {
			t.Fatalf("the refusal must name the flag: %v", err)
		}
		// The refusal leaves the file exactly as it was.
		if b, err := os.ReadFile(override); err != nil || !bytes.Equal(b, userValues) {
			t.Errorf("the refused override changed: (%q, %v), want the original bytes untouched", b, err)
		}
	})
	t.Run("a cluster record inside the state tree, named by --central-values", func(t *testing.T) {
		cfg, tree := newStateTree(t, "refuse-central")
		dir := filepath.Join(tree, "clusters", cfg.Prefix+"-1")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		override := filepath.Join(dir, "record.json")
		if err := os.WriteFile(override, userValues, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := checkValuesOverrides(override, "", cfg.StateRoot, cfg.Prefix)
		if err == nil || !strings.Contains(err.Error(), "state directory") {
			t.Fatalf("checkValuesOverrides inside the state directory = %v, want a refusal naming the state directory", err)
		}
		if !strings.Contains(err.Error(), "--central-values") {
			t.Fatalf("the refusal must name the flag: %v", err)
		}
	})
	t.Run("a case-colliding spelling of the state tree", func(t *testing.T) {
		base := t.TempDir()
		if !caseInsensitiveVolume(t, base) {
			t.Skip("the volume resolves names case-sensitively; a case-colliding spelling cannot denote one directory here")
		}
		cfg := mustWorkConfig(t, base, "casetest")
		if err := os.MkdirAll(filepath.Join(cfg.StateRoot, cfg.Prefix), 0o700); err != nil {
			t.Fatal(err)
		}
		// Spell the state tree's own prefix directory in the other
		// case: on this volume both spellings denote the one directory
		// while remaining two strings — the shape only an identity
		// comparison catches.
		spelled := filepath.Join(cfg.StateRoot, strings.ToUpper(cfg.Prefix), "my-sensor.yaml")
		if err := os.WriteFile(spelled, userValues, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := checkValuesOverrides("", spelled, cfg.StateRoot, cfg.Prefix)
		if err == nil || !strings.Contains(err.Error(), "state directory") {
			t.Fatalf("checkValuesOverrides through a case-colliding state-tree spelling = %v, want a refusal", err)
		}
	})
	t.Run("a file beside the per-prefix state directory is not refused", func(t *testing.T) {
		// The negative control: the state base holds this prefix's
		// tree (and other prefixes' trees) below it, and a user file
		// in the base itself is outside this run's tree — the run
		// rewrites only its own prefix's directory.
		cfg, _ := newStateTree(t, "outside")
		path := filepath.Join(cfg.StateRoot, "my-sensor.yaml")
		if err := os.WriteFile(path, userValues, 0o600); err != nil {
			t.Fatal(err)
		}
		o, err := checkValuesOverrides("", path, cfg.StateRoot, cfg.Prefix)
		if err != nil || o.sensor == "" {
			t.Fatalf("checkValuesOverrides beside the state directory = (%+v, %v), want acceptance", o, err)
		}
	})
	t.Run("a sibling prefix's state tree is not refused", func(t *testing.T) {
		// This run rewrites its own prefix's tree only; a sibling
		// prefix's tree is another run's.
		cfg, _ := newStateTree(t, "mine")
		other := filepath.Join(cfg.StateRoot, "other-prefix", "my-sensor.yaml")
		if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(other, userValues, 0o600); err != nil {
			t.Fatal(err)
		}
		o, err := checkValuesOverrides("", other, cfg.StateRoot, cfg.Prefix)
		if err != nil || o.sensor == "" {
			t.Fatalf("checkValuesOverrides inside a sibling prefix's tree = (%+v, %v), want acceptance", o, err)
		}
	})
	t.Run("a state-tree candidate that cannot be checked fails closed", func(t *testing.T) {
		// The same shared branch on the state-tree side, pinned beside
		// the workspace side: the state root itself is a regular file,
		// so the per-prefix candidate's stat fails with ENOTDIR while
		// the workspace candidate — a sibling of the file — simply
		// does not exist. The failure is deterministic for every
		// caller, unlike a mode-000 directory a privileged process
		// can still stat through.
		dir := t.TempDir()
		plain := filepath.Join(dir, "plainfile")
		if err := os.WriteFile(plain, []byte("not a directory\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "my-sensor.yaml")
		if err := os.WriteFile(path, userValues, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := checkValuesOverrides("", path, plain, "p")
		if err == nil || !strings.Contains(err.Error(), "state directory") {
			t.Fatalf("checkValuesOverrides with an uncheckable state directory = %v, want a startup error naming it", err)
		}
		if !strings.Contains(err.Error(), "--sensor-values") {
			t.Fatalf("the refusal must name the flag: %v", err)
		}
	})
}

// A values override must be one independent file: a hard link's other
// name can be a file the run rewrites in place — a cluster record or a
// kubeconfig in the state directory, whose writer replaces the shared
// inode's contents — and the startup check cannot enumerate a file's
// other names to tell the safe links from the clobbering ones. Every
// multiply-linked values file is therefore refused before the clusters
// are touched, with the file left exactly as it was; the refusal names
// the remedy, an independent copy (cp, not ln). Directory-identity
// containment cannot see this shape: the override's own name sits
// outside both trees while its inode sits inside one through the link.
func TestCheckValuesOverridesRefusesAHardLinkedFile(t *testing.T) {
	userValues := []byte("image:\n  main:\n    fullRef: my-registry.example.com/stackrox/main:my-branch\n")

	t.Run("hard-linked to a cluster record, outside both trees", func(t *testing.T) {
		// The probe shape: a marked per-prefix state tree holding
		// one cluster record — a file Store.WriteRecord rewrites in
		// place on every run — and a private user file outside both
		// trees hard-linked to it. The tree checks pass (the
		// override's own name is outside), so only the link count can
		// refuse it.
		base := t.TempDir()
		cfg := mustWorkConfig(t, base, "hardlink")
		recordDir := filepath.Join(cfg.StateRoot, cfg.Prefix, "clusters", cfg.Prefix+"-1")
		if err := os.MkdirAll(recordDir, 0o700); err != nil {
			t.Fatal(err)
		}
		user := filepath.Join(t.TempDir(), "my-sensor.yaml")
		if err := os.WriteFile(user, userValues, 0o600); err != nil {
			t.Fatal(err)
		}
		record := filepath.Join(recordDir, "record.json")
		if err := os.Link(user, record); err != nil {
			t.Skipf("the volume does not support hard links (%v); the shared-inode shape cannot be built here", err)
		}
		_, err := checkValuesOverrides("", user, cfg.StateRoot, cfg.Prefix)
		if err == nil || !strings.Contains(err.Error(), "independent copy") {
			t.Fatalf("checkValuesOverrides on a file hard-linked to a cluster record = %v, want a refusal naming the independent-copy remedy", err)
		}
		if !strings.Contains(err.Error(), "--sensor-values") {
			t.Fatalf("the refusal must name the flag: %v", err)
		}
		// Both names still read the original bytes: the refusal is a
		// startup error, never a write.
		for _, p := range []string{user, record} {
			if b, err := os.ReadFile(p); err != nil || !bytes.Equal(b, userValues) {
				t.Errorf("the hard-linked file changed through %s: (%q, %v), want the original bytes untouched", p, b, err)
			}
		}
	})
	t.Run("hard-linked to a generated values file, named by --central-values", func(t *testing.T) {
		// The workspace side rides the same refusal: the generated
		// values files are replaced atomically, but the other names a
		// link can carry are not all so kind, and the check cannot
		// tell — so the generated-values shape is refused like any
		// other. This pins a deliberate
		// choice: no acceptance criterion requires hard-link
		// support, and a future acceptance would be a deliberate
		// change, not drift.
		work := t.TempDir()
		generated := filepath.Join(work, centralValuesFile)
		if err := os.WriteFile(generated, []byte("central: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		user := filepath.Join(t.TempDir(), "my-central.yaml")
		if err := os.Link(generated, user); err != nil {
			t.Skipf("the volume does not support hard links (%v)", err)
		}
		_, err := checkValuesOverrides(user, "", mustStateRoot(t), "p")
		if err == nil || !strings.Contains(err.Error(), "independent copy") {
			t.Fatalf("checkValuesOverrides on a file hard-linked to a generated values file = %v, want a refusal naming the independent-copy remedy", err)
		}
		if !strings.Contains(err.Error(), "--central-values") {
			t.Fatalf("the refusal must name the flag: %v", err)
		}
		if b, err := os.ReadFile(user); err != nil || string(b) != "central: {}\n" {
			t.Errorf("the hard-linked file changed: (%q, %v), want the original bytes untouched", b, err)
		}
	})
	t.Run("a single-link values file outside both trees is not refused", func(t *testing.T) {
		// The negative control: the refusal is scoped to files with
		// more than one name — a plain private file outside both
		// trees still passes.
		path := filepath.Join(t.TempDir(), "my-sensor.yaml")
		if err := os.WriteFile(path, userValues, 0o600); err != nil {
			t.Fatal(err)
		}
		o, err := checkValuesOverrides("", path, mustStateRoot(t), "p")
		if err != nil || o.sensor == "" {
			t.Fatalf("checkValuesOverrides on a single-link file = (%+v, %v), want acceptance", o, err)
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
	if err := os.WriteFile(override, []byte("image: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The override goes through the flag validation first: what the
	// runner receives — and what argv must carry — is the resolved
	// absolute path, never the raw spelling the user typed.
	overrides, err := checkValuesOverrides("", override, mustStateRoot(t), "p")
	if err != nil {
		t.Fatalf("checkValuesOverrides = %v", err)
	}
	if !filepath.IsAbs(overrides.sensor) || filepath.Base(overrides.sensor) != "my-sensor.yaml" {
		t.Fatalf("the override was not resolved to its absolute checked path: %q", overrides.sensor)
	}
	runner := helmRunner{binary: stub}
	err = runner.upgradeInstall(context.Background(),
		"/work/charts/stackrox-secured-cluster-services", "/state/clusters/kind-ocp-2/kubeconfig",
		generated, overrides.sensor, time.Minute)
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
		"-f", generated, "-f", overrides.sensor,
		"--wait", "--timeout", "1m0s",
	}
	if !slices.Equal(args, want) {
		t.Errorf("helm argv = %q, want %q (override after the generated values)", args, want)
	}
}

// A values path containing a comma is refused before helm runs: helm
// parses each -f argument as a comma-separated list (verified against
// helm v4.3.1), so a single checked pathname like "/tmp/weird,name/x"
// would make helm load the different, unchecked files "/tmp/weird" and
// "name/x" — or fail on fragments that do not exist. The guard is what
// makes the private-file check meaningful inside helm, and the stub
// never executing is what proves the refusal happened before the
// handoff, not inside a failed helm run.
func TestUpgradeInstallRefusesACommaInAValuesPath(t *testing.T) {
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

	generated := filepath.Join(dir, "weird,name") + "/values.yaml"
	runner := helmRunner{binary: stub}
	err := runner.upgradeInstall(context.Background(),
		"/work/charts/stackrox-central-services", "/state/clusters/kind-ocp-1/kubeconfig",
		generated, "", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "comma") {
		t.Fatalf("upgradeInstall = %v, want a comma refusal", err)
	}
	if _, statErr := os.Lstat(argsFile); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("helm ran despite the refusal (argv file: %v)", statErr)
	}
}

// helm's own output is never surfaced: its diagnostics quote
// values-file content verbatim (a malformed YAML line is echoed back in
// the error), and the values files carry the admin password and the
// cluster registration secret. The stub prints a marker that stands for
// that content on both streams and fails; the returned error must
// carry the command and the exit, never the marker.
func TestUpgradeInstallDoesNotSurfaceHelmOutput(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX sh available")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "helm")
	script := "#!/bin/sh\n" +
		"echo 'leaked: s3cret-admin-password' \n" +
		"echo 'leaked: s3cret-admin-password' >&2 \n" +
		"exit 3\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(dir, "values-central.yaml")
	if err := os.WriteFile(generated, []byte("central: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := helmRunner{binary: stub}
	err := runner.upgradeInstall(context.Background(),
		"/work/charts/stackrox-central-services", "/state/clusters/kind-ocp-1/kubeconfig",
		generated, "", time.Minute)
	if err == nil {
		t.Fatal("a failing helm must fail the install")
	}
	if strings.Contains(err.Error(), "s3cret-admin-password") {
		t.Fatalf("the failure surfaced helm's output: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("the failure does not report the exit status: %v", err)
	}
	// The command line must be copy-safe: every argument and the helm
	// binary itself shell-quoted, so a path with spaces or shell
	// metacharacters cannot change what a pasted re-run does.
	if !strings.Contains(err.Error(), "'upgrade' '--install'") {
		t.Errorf("the failure does not report the shell-quoted phase and command line: %v", err)
	}
	if !strings.Contains(err.Error(), shellQuote(stub)) {
		t.Errorf("the failure does not report the shell-quoted helm binary: %v", err)
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

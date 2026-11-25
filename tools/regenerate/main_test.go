package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// committedAssetsDir locates the committed assets relative to this test
// file, wherever the module is checked out.
func committedAssetsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locating the test file")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "assets", "platform-objects")
}

// fixtureBytes reads one recorded upstream fixture.
func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locating the test file")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "testdata", name))
	if err != nil {
		t.Fatalf("reading the %s fixture: %v (run `make regenerate` to record it)", name, err)
	}
	return b
}

// recordedFixtures loads every recorded fixture, keyed by fixture name.
func recordedFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	fetched := map[string][]byte{}
	for _, name := range fixtureNames {
		fetched[name] = fixtureBytes(t, name)
	}
	return fetched
}

// fixtureTransport serves recorded fixture bytes at the exact URLs the
// pins address — the offline stand-in for raw.githubusercontent.com the
// run-level tests drive run through.
type fixtureTransport struct {
	responses map[string][]byte
}

func (t fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, ok := t.responses[req.URL.String()]
	status := http.StatusOK
	if !ok {
		body = []byte("no fixture recorded for " + req.URL.String())
		status = http.StatusNotFound
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/yaml"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}, nil
}

// replayFixtures builds the transport serving the recorded fixtures at
// the URLs the pins address.
func replayFixtures(t *testing.T) http.RoundTripper {
	t.Helper()
	return fixtureTransportFor(recordedFixtures(t))
}

// fixtureTransportFor serves the fetched set at the exact URLs the pins
// address — recorded fixtures verbatim, or a mutated copy a regression
// wants the command path to see.
func fixtureTransportFor(fetched map[string][]byte) http.RoundTripper {
	return fixtureTransport{responses: map[string][]byte{
		rawURL(kasRepo, kasCommit, defaultConfigPath):                             fetched["kas-defaultconfig.yaml"],
		rawURL(kcmRepo, kcmCommit, defaultConfigPath):                             fetched["kcm-defaultconfig.yaml"],
		rawURL(kasRepo, kasCommit, fmt.Sprintf(sccPathTemplate, "restricted-v2")): fetched["scc-restricted-v2.yaml"],
		rawURL(kasRepo, kasCommit, fmt.Sprintf(sccPathTemplate, "privileged")):    fetched["scc-privileged.yaml"],
	}}
}

// TestRun drives the command path end to end, offline: the recorded
// fixtures stand in for the pinned upstream fetches.
func TestRun(t *testing.T) {
	// copyCommittedAssets copies the committed assets into a fresh root
	// and returns it.
	copyCommittedAssets := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		dir := filepath.Join(root, "assets", "platform-objects")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{objectsFile, sccsFile} {
			b, err := os.ReadFile(filepath.Join(committedAssetsDir(t), name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	readAsset := func(root, name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, "assets", "platform-objects", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// assertUntouched pins the failed-run guarantee: neither asset
	// changed and no fixture appeared.
	assertUntouched := func(t *testing.T, root string, before map[string]string) {
		t.Helper()
		for name, want := range before {
			if got := readAsset(root, name); got != want {
				t.Errorf("the failed run changed %s", name)
			}
		}
		entries, err := os.ReadDir(filepath.Join(root, "tools", "regenerate", "testdata"))
		if err == nil && len(entries) > 0 {
			t.Errorf("the failed run created fixtures: %v", entries)
		} else if err != nil && !os.IsNotExist(err) {
			t.Fatalf("reading the fixtures directory: %v", err)
		}
	}
	// moveKASMarkersToAPIServerConfig lifts the KAS marker pair out of
	// the openshift-kube-apiserver/config ConfigMap and drops it
	// around the hand-maintained openshift-apiserver/config blob: the
	// misplaced-marker mutation, applied to the committed objects.yaml
	// bytes. The pair stays well-formed, so only the splice's
	// destination binding can refuse it.
	moveKASMarkersToAPIServerConfig := func(t *testing.T, b []byte) []byte {
		t.Helper()
		lines := strings.SplitAfter(string(b), "\n")
		kasBegin, kasEnd := -1, -1
		for i, line := range lines {
			if kasBegin < 0 && strings.Contains(line, "# BEGIN UPSTREAM: "+kasRepo+"@") {
				kasBegin = i
				continue
			}
			if kasBegin >= 0 && strings.TrimSpace(line) == endMarker {
				kasEnd = i
				break
			}
		}
		if kasBegin < 0 || kasEnd < 0 {
			t.Fatal("locating the KAS marker pair in objects.yaml")
		}
		oaNS, oaData, oaBlob := -1, -1, -1
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if oaNS < 0 && trimmed == "namespace: openshift-apiserver" {
				oaNS = i
				continue
			}
			if oaNS >= 0 && oaData < 0 && trimmed == "data:" {
				oaData = i
				continue
			}
			if oaData >= 0 && strings.HasPrefix(trimmed, "{") {
				oaBlob = i
				break
			}
		}
		if oaNS < 0 || oaData < 0 || oaBlob < 0 {
			t.Fatal("locating the openshift-apiserver/config blob in objects.yaml")
		}
		var out []string
		for i, line := range lines {
			if i == kasBegin || i == kasEnd {
				continue // lifted; re-inserted around the OA blob
			}
			out = append(out, line)
			if i == oaData {
				out = append(out, lines[kasBegin])
			}
			if i == oaBlob {
				out = append(out, lines[kasEnd])
			}
		}
		return []byte(strings.Join(out, ""))
	}

	t.Run("is a no-op on in-sync pins and records the fixtures", func(t *testing.T) {
		root := copyCommittedAssets(t)
		if err := run(context.Background(), replayFixtures(t), root); err != nil {
			t.Fatalf("run: %v", err)
		}
		for _, name := range []string{objectsFile, sccsFile} {
			if got := readAsset(root, name); got != string(fixtureBytesOrCommitted(t, name)) {
				t.Errorf("run rewrote %s on in-sync pins; the committed file is not what the pins produce — run `make regenerate`", name)
			}
		}
		// The fetched bytes are recorded as the offline fixtures.
		for _, name := range fixtureNames {
			b, err := os.ReadFile(filepath.Join(root, "tools", "regenerate", "testdata", name))
			if err != nil {
				t.Errorf("run did not record the %s fixture: %v", name, err)
				continue
			}
			if string(b) != string(fixtureBytes(t, name)) {
				t.Errorf("the recorded %s fixture differs from the committed one", name)
			}
		}
	})

	t.Run("a root whose objects.yaml lacks the final newline is a no-op", func(t *testing.T) {
		// The splice replaces byte ranges; the run may not normalize
		// the untouched bytes — including appending the newline the
		// file does not end with. The command path, not only the
		// splice, is pinned here: the candidate it builds over
		// in-sync pins must equal the stripped file byte for byte,
		// so nothing is written at all.
		root := copyCommittedAssets(t)
		path := filepath.Join(root, "assets", "platform-objects", objectsFile)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasSuffix(b, []byte("\n")) {
			t.Fatal("the committed objects.yaml already lacks a final newline; the fixture assumption is stale")
		}
		stripped := bytes.TrimSuffix(b, []byte("\n"))
		if err := os.WriteFile(path, stripped, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := run(context.Background(), replayFixtures(t), root); err != nil {
			t.Fatalf("run: %v", err)
		}
		if got := readAsset(root, objectsFile); got != string(stripped) {
			t.Errorf("run rewrote objects.yaml over in-sync pins; a root without a final newline must stay exactly as it is (still ends without one: %t)", !strings.HasSuffix(got, "\n"))
		}
	})

	t.Run("a late validation failure changes and creates no files", func(t *testing.T) {
		root := copyCommittedAssets(t)
		// Corrupt the BEGIN markers so the objects.yaml splice — the
		// last thing run validates — fails after the SCC candidate has
		// already built and validated. Under the ordering this pins,
		// that late failure must not touch the assets or record any
		// fixtures.
		path := filepath.Join(root, "assets", "platform-objects", objectsFile)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.ReplaceAll(b, []byte("# BEGIN UPSTREAM:"), []byte("# begin upstream:")), 0o644); err != nil {
			t.Fatal(err)
		}
		before := map[string]string{objectsFile: readAsset(root, objectsFile), sccsFile: readAsset(root, sccsFile)}
		err = run(context.Background(), replayFixtures(t), root)
		if err == nil {
			t.Fatal("run succeeded over corrupted markers, want a validation error")
		}
		if !strings.Contains(err.Error(), "marker") {
			t.Errorf("run error = %v, want it to name the marker problem", err)
		}
		assertUntouched(t, root, before)
	})

	t.Run("markers moved to another ConfigMap change and create no files", func(t *testing.T) {
		// The misplaced-marker probe: the KAS pair, dropped around the
		// hand-maintained openshift-apiserver/config blob. The pair
		// parses and manages the right field, and that blob carries a
		// replaceable apiServerArguments key — without the destination
		// binding this run splices the KAS map into the wrong
		// ConfigMap and exits 0. The binding must refuse it before
		// anything is written.
		root := copyCommittedAssets(t)
		path := filepath.Join(root, "assets", "platform-objects", objectsFile)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, moveKASMarkersToAPIServerConfig(t, b), 0o644); err != nil {
			t.Fatal(err)
		}
		before := map[string]string{objectsFile: readAsset(root, objectsFile), sccsFile: readAsset(root, sccsFile)}
		err = run(context.Background(), replayFixtures(t), root)
		if err == nil {
			t.Fatal("run succeeded over markers bounding another ConfigMap's content, want a destination-binding error")
		}
		for _, want := range []string{"do not sit immediately outside the config.yaml block of the openshift-kube-apiserver/config ConfigMap", "markers no longer bound their destination"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("run error = %v, want it to say %q", err, want)
			}
		}
		assertUntouched(t, root, before)
	})

	// kasBlockLines locates the KAS marker pair and the config.yaml
	// block it bounds in objects.yaml bytes: the BEGIN line, the
	// config.yaml header, the payload line, and the END line, as
	// indexes into lines (lines from strings.SplitAfter, terminators
	// kept).
	kasBlockLines := func(t *testing.T, lines []string) (begin, header, payload, end int) {
		t.Helper()
		begin = -1
		for i, line := range lines {
			if strings.Contains(line, "# BEGIN UPSTREAM: "+kasRepo+"@") {
				begin = i
				break
			}
		}
		if begin < 0 || begin+3 >= len(lines) {
			t.Fatal("locating the KAS marker pair in objects.yaml")
		}
		header, payload, end = begin+1, begin+2, begin+3
		if !strings.Contains(lines[header], "config.yaml: |") ||
			strings.Contains(lines[payload], "config.yaml:") ||
			strings.TrimSpace(lines[end]) != endMarker {
			t.Fatalf("the KAS block does not have the expected shape: header %q payload %q end %q",
				lines[header], lines[payload], lines[end])
		}
		return begin, header, payload, end
	}

	// quoteAsYAMLDoubleQuoted renders one JSON line as a YAML
	// double-quoted scalar with four escaped leading newlines: still
	// the same JSON after unescaping (a newline is JSON whitespace),
	// but one physical line decoded with five.
	quoteAsYAMLDoubleQuoted := func(json string) string {
		escaped := strings.ReplaceAll(json, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		return `  config.yaml: "` + `\n\n\n\n` + escaped + `"` + "\n"
	}

	t.Run("markers around a sibling over a quoted config.yaml change and create no files", func(t *testing.T) {
		// The quoted-sibling probe: the in-sync config.yaml rewritten
		// as a double-quoted scalar (one physical line, escaped
		// newlines), its markers dropped around a hand-maintained
		// maintainer.yaml block carrying a stale apiServerArguments
		// map. The old span arithmetic counted the decoded scalar's
		// newlines as physical lines, over-claimed past the quoted
		// line onto the sibling, and spliced the pinned map into the
		// sibling's content with exit 0 — the post-validation read the
		// still-in-sync quoted config.yaml and passed. The exact
		// layout binding must refuse the shape before anything is
		// written.
		root := copyCommittedAssets(t)
		path := filepath.Join(root, "assets", "platform-objects", objectsFile)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.SplitAfter(string(b), "\n")
		begin, _, payload, end := kasBlockLines(t, lines)
		quoted := []string{
			quoteAsYAMLDoubleQuoted(strings.TrimRight(lines[payload], "\n")),
			lines[begin],
			"  maintainer.yaml: |-\n",
			"    {\"apiServerArguments\":{\"stale\":[\"1\"]}}\n",
			lines[end],
		}
		var out []string
		out = append(out, lines[:begin]...)
		out = append(out, quoted...)
		out = append(out, lines[end+1:]...)
		if err := os.WriteFile(path, []byte(strings.Join(out, "")), 0o644); err != nil {
			t.Fatal(err)
		}
		before := map[string]string{objectsFile: readAsset(root, objectsFile), sccsFile: readAsset(root, sccsFile)}
		err = run(context.Background(), replayFixtures(t), root)
		if err == nil {
			t.Fatal("run succeeded over markers bounding a sibling next to a quoted config.yaml, want a destination-binding error")
		}
		for _, want := range []string{"not a literal block scalar", "openshift-kube-apiserver/config"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("run error = %v, want it to say %q", err, want)
			}
		}
		assertUntouched(t, root, before)
	})

	t.Run("a BEGIN marker swallowed into a sibling block changes and creates no files", func(t *testing.T) {
		// The swallowed-BEGIN probe: a maintainer.yaml block inserted
		// above the destination, the KAS BEGIN marker (carrying a
		// stale pin) indented into that block's content — still the
		// line above the config.yaml header, so it parses and pairs
		// with the END below the payload. Without the indentation
		// binding the run rewrites the marker line, silently changing
		// the hand-maintained maintainer block's data, and exits 0.
		// A marker inside a sibling block's content is not a comment
		// outside the destination; the binding must refuse it.
		root := copyCommittedAssets(t)
		path := filepath.Join(root, "assets", "platform-objects", objectsFile)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.SplitAfter(string(b), "\n")
		begin, _, _, _ := kasBlockLines(t, lines)
		stalePin := "0000000000000000000000000000000000000000"
		swallowedBegin := strings.Replace(lines[begin], kasCommit, stalePin, 1)
		swallowedBegin = "    " + strings.TrimLeft(swallowedBegin, " ")
		var out []string
		out = append(out, lines[:begin]...)
		out = append(out,
			"  maintainer.yaml: |-\n",
			"    some manual content\n",
			swallowedBegin,
		)
		out = append(out, lines[begin+1:]...)
		if err := os.WriteFile(path, []byte(strings.Join(out, "")), 0o644); err != nil {
			t.Fatal(err)
		}
		before := map[string]string{objectsFile: readAsset(root, objectsFile), sccsFile: readAsset(root, sccsFile)}
		err = run(context.Background(), replayFixtures(t), root)
		if err == nil {
			t.Fatal("run succeeded over a BEGIN marker swallowed into a sibling block, want a destination-binding error")
		}
		for _, want := range []string{"is indented", "not a comment outside the block"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("run error = %v, want it to say %q", err, want)
			}
		}
		assertUntouched(t, root, before)
	})

	t.Run("a duplicated config.yaml key changes and creates no files", func(t *testing.T) {
		// The duplicate-key probe: a second config.yaml entry under
		// the same data mapping, the marker pair bounding the first
		// (stale) copy while the last copy stays in sync. The managed
		// lookup path picked the first — the splice rewrote it — and
		// the post-validation read the last, so the run exited 0
		// having committed a duplicate key with one copy rewritten.
		// The lookup must refuse the ambiguity before anything is
		// written.
		root := copyCommittedAssets(t)
		path := filepath.Join(root, "assets", "platform-objects", objectsFile)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.SplitAfter(string(b), "\n")
		_, header, payload, end := kasBlockLines(t, lines)
		var out []string
		out = append(out, lines[:payload]...)
		out = append(out, "    {\"apiServerArguments\":{\"stale\":[\"1\"]}}\n")
		out = append(out, lines[payload+1:end+1]...)
		out = append(out, lines[header], lines[payload])
		out = append(out, lines[end+1:]...)
		if err := os.WriteFile(path, []byte(strings.Join(out, "")), 0o644); err != nil {
			t.Fatal(err)
		}
		before := map[string]string{objectsFile: readAsset(root, objectsFile), sccsFile: readAsset(root, sccsFile)}
		err = run(context.Background(), replayFixtures(t), root)
		if err == nil {
			t.Fatal("run succeeded over a duplicated config.yaml key, want a destination-binding error")
		}
		for _, want := range []string{`mapping key "config.yaml" appears twice`, "openshift-kube-apiserver/config"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("run error = %v, want it to say %q", err, want)
			}
		}
		assertUntouched(t, root, before)
	})

	t.Run("an alias to the managed config.yaml changes and creates no files", func(t *testing.T) {
		// The alias probe: an anchor on the destination's config.yaml
		// block, a sibling maintainer.yaml key aliasing it, and a stale
		// payload. The splice rewrites only the payload line — the
		// alias's own bytes never move — but the sibling decodes from
		// the spliced block, so its data changes with the splice. The
		// old validation read only the destination's config.yaml and
		// exited 0. The decoded before/after comparison must refuse:
		// a changed sibling is a change outside the managed field.
		root := copyCommittedAssets(t)
		path := filepath.Join(root, "assets", "platform-objects", objectsFile)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.SplitAfter(string(b), "\n")
		begin := -1
		for i, line := range lines {
			if strings.Contains(line, "# BEGIN UPSTREAM: "+kasRepo+"@") {
				begin = i
				break
			}
		}
		if begin < 0 || begin+3 >= len(lines) {
			t.Fatal("locating the KAS marker pair in objects.yaml")
		}
		header, _, end := begin+1, begin+2, begin+3
		if !strings.Contains(lines[header], "config.yaml:") || strings.TrimSpace(lines[end]) != endMarker {
			t.Fatalf("the KAS block does not have the expected shape: header %q end %q", lines[header], lines[end])
		}
		var out []string
		out = append(out, lines[:header]...)
		out = append(out,
			"  config.yaml: &kasConfig |-\n",
			`    {"apiServerArguments":{"stale":["1"]}}`+"\n",
			lines[end],
			"  maintainer.yaml: *kasConfig\n",
		)
		out = append(out, lines[end+1:]...)
		if err := os.WriteFile(path, []byte(strings.Join(out, "")), 0o644); err != nil {
			t.Fatal(err)
		}
		before := map[string]string{objectsFile: readAsset(root, objectsFile), sccsFile: readAsset(root, sccsFile)}
		err = run(context.Background(), replayFixtures(t), root)
		if err == nil {
			t.Fatal("run succeeded over a sibling aliasing the managed config.yaml, want an error")
		}
		for _, want := range []string{"beyond its managed", "data.maintainer.yaml"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("run error = %v, want it to say %q", err, want)
			}
		}
		assertUntouched(t, root, before)
	})

	t.Run("a second ConfigMap under a destination name changes and creates no files", func(t *testing.T) {
		// The duplicate-destination probe: an appended core/v1
		// ConfigMap under the same namespace and name as the KAS
		// destination, carrying a data map without config.yaml. The
		// old destination walk only counted copies that carried a
		// config.yaml, so the appended copy slipped past the duplicate
		// check and the run exited 0 — leaving two objects the layer
		// applies in order under one field manager. The destination
		// identity must be unique whether or not a copy carries
		// config.yaml.
		root := copyCommittedAssets(t)
		path := filepath.Join(root, "assets", "platform-objects", objectsFile)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out := append(strings.SplitAfter(string(b), "\n"),
			"---\n",
			"apiVersion: v1\n",
			"kind: ConfigMap\n",
			"metadata:\n",
			"  name: config\n",
			"  namespace: openshift-kube-apiserver\n",
			"data:\n",
			"  unrelated: also-unrelated\n",
		)
		if err := os.WriteFile(path, []byte(strings.Join(out, "")), 0o644); err != nil {
			t.Fatal(err)
		}
		before := map[string]string{objectsFile: readAsset(root, objectsFile), sccsFile: readAsset(root, sccsFile)}
		err = run(context.Background(), replayFixtures(t), root)
		if err == nil {
			t.Fatal("run succeeded over a second ConfigMap under a destination name, want an error")
		}
		for _, want := range []string{"two ConfigMaps named openshift-kube-apiserver/config", "destination is ambiguous"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("run error = %v, want it to say %q", err, want)
			}
		}
		assertUntouched(t, root, before)
	})

	t.Run("a duplicated managed key in the blob changes and creates no files", func(t *testing.T) {
		// The duplicate-JSON-key probe: the destination's payload line
		// carrying the managed key twice — a stale copy first, the
		// in-sync one last. JSON decoding into a map keeps the last of
		// two same-named keys, so the old splice rewrote the shadowed
		// first copy while its post-validation read the last: the run
		// exited 0 having rewritten bytes no reader sees. The managed
		// key must occur exactly once before the splice rewrites it.
		root := copyCommittedAssets(t)
		path := filepath.Join(root, "assets", "platform-objects", objectsFile)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.SplitAfter(string(b), "\n")
		_, _, payload, _ := kasBlockLines(t, lines)
		line := strings.TrimRight(lines[payload], "\n")
		if !strings.Contains(line, `"apiServerArguments": {`) {
			t.Fatal("the KAS payload does not carry the expected apiServerArguments key")
		}
		lines[payload] = strings.Replace(line, `"apiServerArguments": {`,
			`"apiServerArguments": {"stale":["1"]}, "apiServerArguments": {`, 1) + "\n"
		if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644); err != nil {
			t.Fatal(err)
		}
		before := map[string]string{objectsFile: readAsset(root, objectsFile), sccsFile: readAsset(root, sccsFile)}
		err = run(context.Background(), replayFixtures(t), root)
		if err == nil {
			t.Fatal("run succeeded over a duplicated managed key in the blob, want an error")
		}
		if !strings.Contains(err.Error(), `key "apiServerArguments" appears twice`) {
			t.Errorf("run error = %v, want it to name the duplicated key", err)
		}
		assertUntouched(t, root, before)
	})

	t.Run("is a no-op over a valid untouched number outside float64 range", func(t *testing.T) {
		// The float64 probe: a hand-maintained sibling key in the
		// marked payload carrying 1e309 — valid JSON that no Go
		// float64 can represent. The managed field stays in sync, so
		// the run is a no-op; the validation must not decode the
		// blob's unmanaged values into Go values to compare them,
		// because that decoding alone calls this file invalid JSON
		// and refuses a run that changes nothing. Unmanaged blob
		// keys are compared as raw bytes, never decoded.
		root := copyCommittedAssets(t)
		path := filepath.Join(root, "assets", "platform-objects", objectsFile)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.SplitAfter(string(b), "\n")
		_, _, payload, _ := kasBlockLines(t, lines)
		line := strings.TrimRight(lines[payload], "\n")
		if !strings.Contains(line, `"kind": "KubeAPIServerConfig"`) {
			t.Fatal("the KAS payload does not carry the expected kind key")
		}
		lines[payload] = strings.Replace(line, `"kind": "KubeAPIServerConfig"`,
			`"maintainerNumber": 1e309, "kind": "KubeAPIServerConfig"`, 1) + "\n"
		mutated := []byte(strings.Join(lines, ""))
		if err := os.WriteFile(path, mutated, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := run(context.Background(), replayFixtures(t), root); err != nil {
			t.Fatalf("run refused a payload carrying a valid untouched number outside float64 range: %v", err)
		}
		if got := readAsset(root, objectsFile); got != string(mutated) {
			t.Errorf("run rewrote objects.yaml over in-sync pins; the untouched number must keep its original bytes")
		}
	})

	t.Run("a config.yaml in another shape on an unmanaged ConfigMap is left alone", func(t *testing.T) {
		// The unmanaged-layout probe: the hand-maintained
		// openshift-apiserver/config config.yaml re-rendered as a
		// double-quoted scalar — same decoded JSON, a shape the splice
		// does not support. The old destination walk imposed its
		// layout on every ConfigMap in the file and refused. The exact
		// layout is the two splice destinations' business alone; the
		// run must stay a no-op over in-sync pins.
		root := copyCommittedAssets(t)
		path := filepath.Join(root, "assets", "platform-objects", objectsFile)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.SplitAfter(string(b), "\n")
		oaNS, oaData, oaBlob := -1, -1, -1
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if oaNS < 0 && trimmed == "namespace: openshift-apiserver" {
				oaNS = i
				continue
			}
			if oaNS >= 0 && oaData < 0 && trimmed == "data:" {
				oaData = i
				continue
			}
			if oaData >= 0 && strings.HasPrefix(trimmed, "{") {
				oaBlob = i
				break
			}
		}
		if oaNS < 0 || oaData < 0 || oaBlob != oaData+2 {
			t.Fatalf("locating the openshift-apiserver/config blob in objects.yaml: ns %d data %d blob %d", oaNS, oaData, oaBlob)
		}
		lines[oaBlob-1] = quoteAsYAMLDoubleQuoted(strings.TrimRight(lines[oaBlob], "\n"))
		lines[oaBlob] = ""
		mutated := []byte(strings.Join(lines, ""))
		if err := os.WriteFile(path, mutated, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := run(context.Background(), replayFixtures(t), root); err != nil {
			t.Fatalf("run refused a file whose unmanaged config.yaml is a quoted scalar: %v", err)
		}
		if got := readAsset(root, objectsFile); got != string(mutated) {
			t.Errorf("run rewrote objects.yaml over in-sync pins; the quoted unmanaged config.yaml must stay exactly as it is")
		}
	})

	t.Run("an in-sync sccs.yaml missing a CRD-required field changes and creates no files", func(t *testing.T) {
		// Remove a non-nullable required key from BOTH the committed
		// sccs.yaml copy and the served restricted-v2 fixture: the
		// assembled candidate then matches the on-disk file — a run
		// with no required-field check would be a write-free no-op
		// and exit 0. The CRD-required validation must refuse it
		// whatever the committed asset says.
		root := copyCommittedAssets(t)
		strip := func(b []byte) []byte {
			return []byte(strings.Replace(string(b), "allowHostIPC: false\n", "", 1))
		}
		path := filepath.Join(root, "assets", "platform-objects", sccsFile)
		if err := os.WriteFile(path, strip([]byte(readAsset(root, sccsFile))), 0o644); err != nil {
			t.Fatal(err)
		}
		fetched := recordedFixtures(t)
		fetched["scc-restricted-v2.yaml"] = strip(fetched["scc-restricted-v2.yaml"])
		before := map[string]string{objectsFile: readAsset(root, objectsFile), sccsFile: readAsset(root, sccsFile)}
		err := run(context.Background(), fixtureTransportFor(fetched), root)
		if err == nil {
			t.Fatal("run succeeded over an SCC stream missing a CRD-required field, want a validation error")
		}
		if !strings.Contains(err.Error(), "allowHostIPC") {
			t.Errorf("run error = %v, want it to name the missing field", err)
		}
		assertUntouched(t, root, before)
	})
}

// fixtureBytesOrCommitted reads a committed asset by name.
func fixtureBytesOrCommitted(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(committedAssetsDir(t), name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Synthetic upstream fixtures shaped like the real ones, small enough
// to read: one SCC manifest per file, one default config per operator.
// The synthetic SCCs carry every field the pinned CRD marks required
// (the real manifests do too), so the assembled stream passes the
// required-field validation the command path enforces.
const (
	syntheticRestricted = `allowHostDirVolumePlugin: false
allowHostIPC: false
allowHostNetwork: false
allowHostPID: false
allowHostPorts: false
allowPrivilegedContainer: false
allowedCapabilities:
apiVersion: security.openshift.io/v1
defaultAddCapabilities:
kind: SecurityContextConstraints
metadata:
  name: restricted-v2
priority:
readOnlyRootFilesystem: false
requiredDropCapabilities:
volumes:
- configMap
`
	syntheticPrivileged = `allowHostDirVolumePlugin: true
allowHostIPC: true
allowHostNetwork: true
allowHostPID: true
allowHostPorts: true
allowPrivilegedContainer: true
allowedCapabilities:
- '*'
apiVersion: security.openshift.io/v1
defaultAddCapabilities:
kind: SecurityContextConstraints
metadata:
  name: privileged
priority:
readOnlyRootFilesystem: false
requiredDropCapabilities:
volumes:
- '*'
`
	syntheticKASConfig = `apiVersion: kubecontrolplane.config.openshift.io/v1
kind: KubeAPIServerConfig
apiServerArguments:
  allow-privileged:
  - "true"
  audit-log-format:
  - json
`
	syntheticKCMConfig = `apiVersion: kubecontrolplane.config.openshift.io/v1
kind: KubeControllerManagerConfig
extendedArguments:
  leader-elect:
  - "true"
  controllers:
  - "*"
`
)

// syntheticFetched mirrors the real fetch set with the synthetic
// fixtures above.
func syntheticFetched() map[string][]byte {
	return map[string][]byte{
		"scc-restricted-v2.yaml": []byte(syntheticRestricted),
		"scc-privileged.yaml":    []byte(syntheticPrivileged),
		"kas-defaultconfig.yaml": []byte(syntheticKASConfig),
		"kcm-defaultconfig.yaml": []byte(syntheticKCMConfig),
	}
}

// syntheticObjects is an objects.yaml stand-in carrying both marker
// pairs, one spaced blob and one compact, with sibling keys around the
// spliced fields that must survive byte-identical. Between the two
// marked ConfigMaps sits the hand-maintained openshift-apiserver/config
// one — no markers of its own, a config.yaml blob that happens to carry
// an apiServerArguments key — the shape a misplaced marker pair lands
// on.
const syntheticObjects = `apiVersion: v1
kind: Namespace
metadata:
  name: openshift-kube-apiserver
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config
  namespace: openshift-kube-apiserver
data:
  # BEGIN UPSTREAM: cluster-kube-apiserver-operator@0000000000000000000000000000000000000000 bindata/assets/config/defaultconfig.yaml (apiServerArguments)
  config.yaml: |-
    {"apiVersion": "kubecontrolplane.config.openshift.io/v1", "kind": "KubeAPIServerConfig", "admission": {"pluginConfig": {}}, "apiServerArguments": {"old-flag": ["0"]}, "servingInfo": {"minTLSVersion": "VersionTLS12"}}
  # END UPSTREAM
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config
  namespace: openshift-apiserver
data:
  config.yaml: |-
    {"apiVersion":"openshiftcontrolplane.config.openshift.io/v1","kind":"OpenShiftAPIServerConfig","apiServerArguments":{"audit-log-format":["json"]}}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config
  namespace: openshift-kube-controller-manager
data:
  # BEGIN UPSTREAM: cluster-kube-controller-manager-operator@0000000000000000000000000000000000000000 bindata/assets/config/defaultconfig.yaml (extendedArguments)
  config.yaml: |-
    {"apiVersion":"kubecontrolplane.config.openshift.io/v1","kind":"KubeControllerManagerConfig","extendedArguments":{"old":["0"]},"servingInfo":{"minTLSVersion":"VersionTLS12"}}
  # END UPSTREAM
`

// syntheticObjectsMovedMarkers is syntheticObjects with the KAS marker
// pair lifted whole and dropped around the hand-maintained
// openshift-apiserver/config blob: the misplaced-marker shape. The pair
// stays a well-formed BEGIN/END pair, so only the destination binding
// can refuse it.
const syntheticObjectsMovedMarkers = `apiVersion: v1
kind: Namespace
metadata:
  name: openshift-kube-apiserver
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config
  namespace: openshift-kube-apiserver
data:
  config.yaml: |-
    {"apiVersion": "kubecontrolplane.config.openshift.io/v1", "kind": "KubeAPIServerConfig", "admission": {"pluginConfig": {}}, "apiServerArguments": {"old-flag": ["0"]}, "servingInfo": {"minTLSVersion": "VersionTLS12"}}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config
  namespace: openshift-apiserver
data:
  # BEGIN UPSTREAM: cluster-kube-apiserver-operator@0000000000000000000000000000000000000000 bindata/assets/config/defaultconfig.yaml (apiServerArguments)
  config.yaml: |-
    {"apiVersion":"openshiftcontrolplane.config.openshift.io/v1","kind":"OpenShiftAPIServerConfig","apiServerArguments":{"audit-log-format":["json"]}}
  # END UPSTREAM
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config
  namespace: openshift-kube-controller-manager
data:
  # BEGIN UPSTREAM: cluster-kube-controller-manager-operator@0000000000000000000000000000000000000000 bindata/assets/config/defaultconfig.yaml (extendedArguments)
  config.yaml: |-
    {"apiVersion":"kubecontrolplane.config.openshift.io/v1","kind":"KubeControllerManagerConfig","extendedArguments":{"old":["0"]},"servingInfo":{"minTLSVersion":"VersionTLS12"}}
  # END UPSTREAM
`

func TestAssembleSCCs(t *testing.T) {
	t.Run("builds the header, both manifests verbatim, and the separator", func(t *testing.T) {
		content, err := assembleSCCs(syntheticFetched())
		if err != nil {
			t.Fatalf("assembleSCCs: %v", err)
		}
		got := string(content)
		if !strings.Contains(got, kasCommit) {
			t.Error("the header does not record the pinned commit")
		}
		if !strings.Contains(got, "tools/regenerate -- do not hand-edit") {
			t.Error("the header does not say the file is regenerated")
		}
		if !strings.Contains(got, syntheticRestricted+"---\n"+syntheticPrivileged) {
			t.Error("the two manifests are not concatenated verbatim around the separator")
		}
	})

	t.Run("refuses an SCC that is not the one it claims to be", func(t *testing.T) {
		for name, fixture := range map[string]string{
			"wrong kind":    strings.Replace(syntheticRestricted, "SecurityContextConstraints", "Pod", 1),
			"wrong name":    strings.Replace(syntheticRestricted, "restricted-v2", "other", 1),
			"two documents": syntheticRestricted + "---\n" + syntheticRestricted,
		} {
			fetched := syntheticFetched()
			fetched["scc-restricted-v2.yaml"] = []byte(fixture)
			if _, err := assembleSCCs(fetched); err == nil {
				t.Errorf("assembleSCCs(%s) succeeded, want a shape-validation error", name)
			}
		}
	})

	t.Run("refuses an assembled stream that lost a CRD-required field", func(t *testing.T) {
		// Upstream dropping a required key must fail at regeneration
		// time, not in every later create run — nullable or not: the
		// check reads the pinned CRD's required set, not just the
		// keys the null-preserving write path is about.
		for name, removed := range map[string]string{
			"nullable field":     "priority:\n",
			"non-nullable field": "readOnlyRootFilesystem: false\n",
		} {
			fetched := syntheticFetched()
			fetched["scc-privileged.yaml"] = []byte(strings.Replace(syntheticPrivileged, removed, "", 1))
			_, err := assembleSCCs(fetched)
			field := strings.TrimSpace(removed[:strings.IndexByte(removed, ':')])
			if err == nil || !strings.Contains(err.Error(), `"`+field+`"`) {
				t.Errorf("assembleSCCs(missing %s) = %v, want an error naming the field", name, err)
			}
		}
	})
}

func TestUpdateObjects(t *testing.T) {
	// writeSynthetic copies the synthetic objects.yaml into dir.
	writeSynthetic := func(t *testing.T, content string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, objectsFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("splices exactly the two fields, leaving every other byte in place", func(t *testing.T) {
		dir := writeSynthetic(t, syntheticObjects)
		updated, err := updateObjects(syntheticFetched(), dir)
		if err != nil {
			t.Fatalf("updateObjects: %v", err)
		}
		got := string(updated)

		// The spaced blob: the new field in place, rendered by the
		// standard encoder (compact); the sibling keys before and after
		// it, and the separator style around the value, byte-identical.
		wantKAS := `    {"apiVersion": "kubecontrolplane.config.openshift.io/v1", "kind": "KubeAPIServerConfig", "admission": {"pluginConfig": {}}, "apiServerArguments": {"allow-privileged":["true"],"audit-log-format":["json"]}, "servingInfo": {"minTLSVersion": "VersionTLS12"}}`
		if !strings.Contains(got, wantKAS) {
			t.Errorf("the spliced KAS blob =\n%s\nwant\n%s", got, wantKAS)
		}
		// The compact blob's siblings stay compact around the new value.
		wantKCM := `    {"apiVersion":"kubecontrolplane.config.openshift.io/v1","kind":"KubeControllerManagerConfig","extendedArguments":{"controllers":["*"],"leader-elect":["true"]},"servingInfo":{"minTLSVersion":"VersionTLS12"}}`
		if !strings.Contains(got, wantKCM) {
			t.Errorf("the spliced KCM blob =\n%s\nwant\n%s", got, wantKCM)
		}
		// The markers carry the pins.
		if !strings.Contains(got, "cluster-kube-apiserver-operator@"+kasCommit) ||
			!strings.Contains(got, "cluster-kube-controller-manager-operator@"+kcmCommit) {
			t.Error("the BEGIN markers do not carry the pinned commits")
		}
		// Nothing else moved: the namespace document is verbatim.
		if !strings.Contains(got, "  name: openshift-kube-apiserver\n") {
			t.Error("the splice disturbed content outside the markers")
		}

		// Re-splicing the result is a no-op.
		if err := os.WriteFile(filepath.Join(dir, objectsFile), updated, 0o644); err != nil {
			t.Fatal(err)
		}
		again, err := updateObjects(syntheticFetched(), dir)
		if err != nil {
			t.Fatalf("second updateObjects: %v", err)
		}
		if string(again) != got {
			t.Error("re-splicing with the same upstream content changed the file; regeneration must be idempotent")
		}
	})

	t.Run("a file without a final newline keeps it that way", func(t *testing.T) {
		// The splice replaces byte ranges; it may not normalize the
		// untouched bytes — including appending the newline the file
		// does not end with.
		dir := writeSynthetic(t, strings.TrimSuffix(syntheticObjects, "\n"))
		updated, err := updateObjects(syntheticFetched(), dir)
		if err != nil {
			t.Fatalf("updateObjects: %v", err)
		}
		if strings.HasSuffix(string(updated), "\n") {
			t.Error("the splice appended a newline outside the marker spans")
		}
		if !strings.Contains(string(updated), `"apiServerArguments": {"allow-privileged":["true"],"audit-log-format":["json"]}`) {
			t.Error("the splice did not happen")
		}
	})

	t.Run("refuses malformed marker structure instead of splicing around it", func(t *testing.T) {
		kasBegin := "  # BEGIN UPSTREAM: cluster-kube-apiserver-operator@0000000000000000000000000000000000000000 bindata/assets/config/defaultconfig.yaml (apiServerArguments)\n"
		kasBlock := syntheticObjects[strings.Index(syntheticObjects, kasBegin) : strings.Index(syntheticObjects, "# END UPSTREAM")+len("# END UPSTREAM\n")]
		for name, content := range map[string]string{
			// A missing END must not be borrowed from the next pair.
			"missing END":       strings.Replace(syntheticObjects, "  # END UPSTREAM\n", "", 1),
			"duplicate pair":    strings.Replace(syntheticObjects, kasBlock, kasBlock+kasBlock, 1),
			"END without BEGIN": strings.Replace(syntheticObjects, kasBegin, "", 1),
			"unmanaged pair":    syntheticObjects + "  # BEGIN UPSTREAM: some-other-repo@0000000000000000000000000000000000000000 some/path.yaml (someField)\n  config.yaml: |-\n    {\"someField\": {\"a\": [\"1\"]}}\n  # END UPSTREAM\n",
			"malformed BEGIN":   strings.Replace(syntheticObjects, kasBegin, "  # BEGIN UPSTREAM: no-payload-here\n", 1),
			"malformed END":     strings.Replace(syntheticObjects, "# END UPSTREAM\n", "# END UPSTREAM and some junk\n", 1),
		} {
			dir := writeSynthetic(t, content)
			_, err := updateObjects(syntheticFetched(), dir)
			if err == nil || !strings.Contains(err.Error(), "marker") {
				t.Errorf("updateObjects(%s) = %v, want an error naming the marker structure", name, err)
			}
		}
	})

	t.Run("refuses markers that bound another ConfigMap's content", func(t *testing.T) {
		// The misplaced-marker shape: a well-formed pair dropped
		// around the hand-maintained openshift-apiserver/config blob,
		// which carries an apiServerArguments key of its own. Only
		// the destination binding can tell it from a real splice —
		// the pair parses, manages the right field, and the blob
		// holds a replaceable key — so this is the pin that the
		// binding, not some incidental error, refuses it.
		dir := writeSynthetic(t, syntheticObjectsMovedMarkers)
		_, err := updateObjects(syntheticFetched(), dir)
		if err == nil {
			t.Fatal("updateObjects over markers bounding another ConfigMap's content succeeded, want a destination-binding error")
		}
		for _, want := range []string{"do not sit immediately outside the config.yaml block of the openshift-kube-apiserver/config ConfigMap", "markers no longer bound their destination"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("updateObjects(moved markers) = %v, want the error to say %q", err, want)
			}
		}
	})

	t.Run("refuses a sibling key aliasing the managed config.yaml", func(t *testing.T) {
		// The alias shape over the synthetic stream: an anchor on the
		// KAS config.yaml, a maintainer.yaml sibling aliasing it. The
		// splice rewrites the payload line — the alias's bytes never
		// move — but the decoded sibling follows the spliced block.
		// The before/after decode must refuse the change outside the
		// managed field.
		mutated := strings.Replace(syntheticObjects, "  config.yaml: |-\n", "  config.yaml: &kasConfig |-\n", 1)
		mutated = strings.Replace(mutated, "  # END UPSTREAM\n", "  # END UPSTREAM\n  maintainer.yaml: *kasConfig\n", 1)
		dir := writeSynthetic(t, mutated)
		_, err := updateObjects(syntheticFetched(), dir)
		if err == nil {
			t.Fatal("updateObjects over a sibling aliasing the managed config.yaml succeeded, want an error")
		}
		for _, want := range []string{"beyond its managed", "data.maintainer.yaml"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("updateObjects(alias) = %v, want the error to say %q", err, want)
			}
		}
	})

	t.Run("splices past a valid number no float64 can represent", func(t *testing.T) {
		// The blob's unmanaged fields are hand-maintained and may
		// carry any valid JSON. A number outside float64 range is
		// valid JSON; comparing it as decoded Go values would call
		// the blob invalid and refuse the splice. The managed field
		// here is stale, so the splice must rewrite it and leave the
		// number's bytes verbatim.
		bigNumber := strings.Replace(syntheticObjects,
			`"kind": "KubeAPIServerConfig"`,
			`"maintainerNumber": 1e309, "kind": "KubeAPIServerConfig"`, 1)
		dir := writeSynthetic(t, bigNumber)
		updated, err := updateObjects(syntheticFetched(), dir)
		if err != nil {
			t.Fatalf("updateObjects: %v", err)
		}
		got := string(updated)
		if !strings.Contains(got, `"maintainerNumber": 1e309, "kind": "KubeAPIServerConfig"`) {
			t.Error("the splice disturbed the untouched number outside float64 range")
		}
		if !strings.Contains(got, `"apiServerArguments": {"allow-privileged":["true"],"audit-log-format":["json"]}`) {
			t.Error("the splice did not replace the managed field")
		}
	})

	t.Run("refuses a file without the markers", func(t *testing.T) {
		dir := writeSynthetic(t, strings.ReplaceAll(syntheticObjects, "# BEGIN UPSTREAM:", "# begin upstream:"))
		if _, err := updateObjects(syntheticFetched(), dir); err == nil {
			t.Fatal("updateObjects without markers succeeded, want an error")
		}
	})

	t.Run("refuses an upstream field with the wrong shape", func(t *testing.T) {
		for name, config := range map[string]string{
			"missing field": "kind: KubeAPIServerConfig\n",
			"scalar field":  "apiServerArguments: true\n",
			"nested object": "apiServerArguments:\n  flag:\n    nested: true\n",
			"null value":    "apiServerArguments:\n  flag:\n  - null\n",
		} {
			fetched := syntheticFetched()
			fetched["kas-defaultconfig.yaml"] = []byte(config)
			dir := writeSynthetic(t, syntheticObjects)
			if _, err := updateObjects(fetched, dir); err == nil {
				t.Errorf("updateObjects(%s) succeeded, want a shape error", name)
			}
		}
	})
}

func TestUpstreamArgumentsCoercion(t *testing.T) {
	config := []byte(`apiServerArguments:
  a-string:
  - hello
  a-bool:
  - true
  a-bool-false:
  - false
  an-int:
  - 10
`)
	args, err := upstreamArguments(config, "repo", "commit", "path", "apiServerArguments")
	if err != nil {
		t.Fatalf("upstreamArguments: %v", err)
	}
	want := map[string][]string{
		"a-string":     {"hello"},
		"a-bool":       {"true"},
		"a-bool-false": {"false"},
		"an-int":       {"10"},
	}
	for k, v := range want {
		if fmt.Sprint(args[k]) != fmt.Sprint(v) {
			t.Errorf("args[%q] = %v, want %v (scalars coerced to their string renderings)", k, args[k], v)
		}
	}
}

func TestReplaceTopLevelField(t *testing.T) {
	t.Run("replaces the field wherever it sits, leaving the rest byte-identical", func(t *testing.T) {
		args := map[string][]string{"new": {"1"}}
		for _, line := range []string{
			`{"apiServerArguments": ["old"], "kind": "K"}`,
			`{"kind": "K", "apiServerArguments": ["old"], "other": 1}`,
			`{"kind": "K", "other": 1, "apiServerArguments": ["old"]}`,
		} {
			got, err := replaceTopLevelField(line, "apiServerArguments", args)
			if err != nil {
				t.Fatalf("replaceTopLevelField(%s): %v", line, err)
			}
			want := strings.Replace(line, `["old"]`, `{"new":["1"]}`, 1)
			if got != want {
				t.Errorf("replaceTopLevelField(%s) = %s, want %s", line, got, want)
			}
		}
	})

	t.Run("retains the original bytes when the decoded value already matches", func(t *testing.T) {
		args := map[string][]string{"a": {"1"}, "b": {"2", "3"}}
		for _, line := range []string{
			// Hand-maintained spacing and key order must survive an
			// in-sync splice untouched.
			`{"apiServerArguments": {"a": ["1"], "b": ["2", "3"]}, "kind": "K"}`,
			`{"kind": "K", "apiServerArguments":{"b":["2","3"],"a":["1"]}}`,
		} {
			got, err := replaceTopLevelField(line, "apiServerArguments", args)
			if err != nil {
				t.Fatalf("replaceTopLevelField(%s): %v", line, err)
			}
			if got != line {
				t.Errorf("replaceTopLevelField(%s) = %s, want the line unchanged (the decoded value already matches)", line, got)
			}
		}
	})

	t.Run("the field name inside another key's value is not the field", func(t *testing.T) {
		line := `{"spec": {"apiServerArguments": "decoy"}, "apiServerArguments": ["real"]}`
		got, err := replaceTopLevelField(line, "apiServerArguments", map[string][]string{"new": {"1"}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, `"decoy"`) || strings.Contains(got, `"real"`) {
			t.Errorf("replaceTopLevelField = %s, want only the top-level field replaced", got)
		}
	})

	t.Run("a duplicated managed key is an error, not a rewrite of the shadowed copy", func(t *testing.T) {
		// Decoding a JSON object into a map keeps the last of two
		// same-named keys; the splice must refuse the duplication
		// rather than rewrite the copy every reader ignores.
		line := `{"apiServerArguments": ["stale"], "kind": "K", "apiServerArguments": ["pinned"]}`
		_, err := replaceTopLevelField(line, "apiServerArguments", map[string][]string{"pinned": {"1"}})
		if err == nil || !strings.Contains(err.Error(), "appears twice") {
			t.Errorf("replaceTopLevelField(duplicated key) = %v, want an error naming the duplication", err)
		}
	})

	t.Run("a missing field and invalid JSON are errors", func(t *testing.T) {
		args := map[string][]string{"new": {"1"}}
		if _, err := replaceTopLevelField(`{"kind": "K"}`, "apiServerArguments", args); err == nil {
			t.Error("missing field succeeded, want an error")
		}
		if _, err := replaceTopLevelField(`not json`, "apiServerArguments", args); err == nil {
			t.Error("invalid JSON succeeded, want an error")
		}
		if _, err := replaceTopLevelField(`[1,2]`, "apiServerArguments", args); err == nil {
			t.Error("a JSON array succeeded, want an error")
		}
	})
}

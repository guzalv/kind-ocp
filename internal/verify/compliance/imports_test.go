package compliance

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// The proof's import boundary: the package this test lives in must
// never be imported by the create path (or anything else that ships
// in the binary) — it deploys an operator, which is a deployment
// proof's job, not a layer's, not the CLI's, not the façade's. The
// two allowed consumers are the tagged integration test under
// test/integration, which runs the proof against clusters the create
// path produced and deletes them after, and the catalog regeneration
// tool under tools/pin-compliance-catalog, the proof's maintenance
// half: it reads the proof's server pin (never its cluster behavior)
// so the tool's verification and the proof's deployment cannot drift
// apart. The tool runs only on a maintainer's machine and is linked
// into nothing that ships.
//
// The test parses every Go file in the module (test files included)
// and fails on any import of this package outside those places.
// A new legitimate consumer would have to change this test — which is
// the point: the boundary is a decision, and decisions get reviewed.
func TestNothingImportsTheProofOutsideItsConsumers(t *testing.T) {
	const self = "github.com/stackrox/kind-ocp/internal/verify/compliance"
	// This package's directory, from which relative paths resolve.
	const repoRoot = "../../.."

	var violations []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip the repository's own machinery and build output; the
		// module's source is what can import the proof.
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "docs", ".opencode":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			imported := strings.Trim(imp.Path.Value, `"`)
			if imported != self {
				continue
			}
			switch {
			case strings.HasPrefix(rel, "internal"+string(filepath.Separator)+"verify"+string(filepath.Separator)+"compliance"+string(filepath.Separator)):
				// The proof itself.
			case strings.HasPrefix(rel, "tools"+string(filepath.Separator)+"pin-compliance-catalog"+string(filepath.Separator)):
				// The catalog regeneration tool: the proof's
				// maintenance half, reading the proof's pins.
			case strings.HasSuffix(path, "_test.go") && strings.HasPrefix(rel, "test"+string(filepath.Separator)+"integration"+string(filepath.Separator)):
				// The tagged integration test, the proof's consumer.
			case strings.HasSuffix(path, "_test.go"):
				violations = append(violations, rel+" (test file outside test/integration)")
			default:
				violations = append(violations, rel+" (shipped code)")
			}
			return nil
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("the compliance proof is imported outside its consumers:\n  %s\nthe proof deploys an operator: it belongs behind the create path's boundary, with only the tagged integration test as its consumer", strings.Join(violations, "\n  "))
	}
}

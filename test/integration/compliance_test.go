//go:build integration

package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/guzalv/kind-ocp/config"
	"github.com/guzalv/kind-ocp/internal/verify/compliance"
)

// TestComplianceOperatorFitness is the CO fitness proof, run per
// cluster: the default count (2) of full-profile clusters is created,
// and on each cluster the proof installs a real Compliance Operator
// through OLM and drives one ocp4-cis scan to real PASS and FAIL
// results — the workload shape this whole repo exists to host, proven
// on every cluster the default configuration builds. The proofs run
// concurrently, like the create wave they follow: the clusters are
// independent, and the wall time is dominated by per-cluster image
// pulls no sequencing would share. Each cluster's lines carry its
// name (the proof logs with it), so interleaved output stays
// attributable.
//
// The proof mutates the clusters it runs on (it deploys an operator
// and runs scans); the clusters are this test's own, deleted in
// cleanup, which is the disposal path.
func TestComplianceOperatorFitness(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	prefix := randomPrefix(t)
	client, cfg := defaultProfileClient(t, prefix, config.DefaultCount)

	if _, err := client.Delete(ctx); err != nil {
		t.Fatalf("pre-clean delete: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Delete(ctx) })

	records, err := client.Create(ctx)
	if err != nil {
		t.Fatalf("create (default count %d, profile %s): %v", cfg.Count, cfg.Profile, err)
	}
	if len(records) != config.DefaultCount {
		t.Fatalf("records = %d, want the default count %d", len(records), config.DefaultCount)
	}

	// One proof per cluster, concurrently. Each failure is kept with
	// its cluster's name; all proofs run to completion so one bad
	// cluster never hides another's outcome — the same contract the
	// create wave's error aggregation follows.
	errs := make([]error, len(records))
	var wg sync.WaitGroup
	for i, rec := range records {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			kubeconfig, err := client.Kubeconfig(ctx, name)
			if err != nil {
				errs[i] = fmt.Errorf("kubeconfig for %s: %w", name, err)
				return
			}
			log := testLogger().With("cluster", name, "check", "compliance-fitness")
			if err := compliance.Prove(ctx, compliance.Options{
				KubeconfigPath: kubeconfig,
				Log:            log,
			}); err != nil {
				errs[i] = fmt.Errorf("cluster %s: %w", name, err)
				return
			}
			t.Logf("cluster %s: the Compliance Operator proved fit (ocp4-cis produced PASS and FAIL results)", name)
		}(i, rec.Name)
	}
	wg.Wait()

	// Report every failing cluster before failing the test: with two
	// clusters, one healthy and one not, the healthy one's pass is
	// evidence worth keeping in the output.
	failed := false
	for _, err := range errs {
		if err != nil {
			failed = true
			t.Errorf("compliance proof: %v", err)
		}
	}
	if failed {
		t.Fatal("the Compliance Operator fitness proof failed on at least one cluster")
	}
}

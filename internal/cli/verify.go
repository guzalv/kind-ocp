package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newVerifyCmd(vals *flagValues, build clientBuilder) *cobra.Command {
	return &cobra.Command{
		Use:   "verify [NAME...]",
		Short: "Run global and per-layer checks against prefix-scoped clusters.",
		Long: `Run every check against the named clusters, or all prefix-scoped clusters
when no names are given: global checks (nodes Ready, cross-cluster NodePort
reachability) and each recorded layer's verify check. With no names, verify
also asserts that every cluster the last create set out to build is still
in the inventory (the count create persisted in its state). Prints one
PASS/FAIL/SKIP line per check and exits non-zero when any check fails.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd, vals)
			if err != nil {
				return err
			}
			log := newLogger(cmd)
			client, err := build(cfg, log)
			if err != nil {
				return err
			}
			results, err := client.Verify(cmd.Context(), args)
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 3, ' ', 0)
			render := func(format string, args ...any) {
				// Rendering errors on a broken pipe are not actionable; the
				// exit code is the failure signal.
				_, _ = fmt.Fprintf(w, format+"\n", args...)
			}
			render("CLUSTER\tCHECK\tSTATUS\tDETAIL")
			failed := false
			for _, r := range results {
				if r.Status == "FAIL" {
					failed = true
				}
				render("%s\t%s\t%s\t%s", r.Cluster, r.Check, r.Status, r.Detail)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if failed {
				return fmt.Errorf("verification failed")
			}
			printfOut(cmd, "all checks passed")
			return nil
		},
	}
}

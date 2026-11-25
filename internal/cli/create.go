package cli

import (
	"github.com/spf13/cobra"
)

func newCreateCmd(vals *flagValues, build clientBuilder) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create N kind clusters and apply the configured layers.",
		Long: `Create the configured clusters (<prefix>-1 .. <prefix>-N) and apply each
cluster's profile. Clusters whose state record matches the requested node
image and profile — and that are the instance the record is bound to — are
reused (reconcile semantics); anything else is rejected with instructions
to delete and re-create.

On failure the run stops, the failing cluster and step are named, and the
clusters are kept for debugging. The printed delete command — which pins
the effective prefix, state root, and an empty --config so it runs verbatim
in any environment — cleans everything up.`,
		Args: cobra.NoArgs,
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

			records, err := client.Create(cmd.Context())
			if err != nil {
				printfErr(cmd, "")
				printfErr(cmd, "create failed: %v", err)
				// Always print an executable cleanup command. Clusters can
				// exist even when this run recorded none (a failure during
				// export or after provisioning, or reuse of earlier state),
				// so records must not gate the hint.
				printfErr(cmd, "Clusters (if any) are kept for debugging. To clean up, run:")
				printfErr(cmd, "  %s", deleteCommand(cfg))
				return err
			}

			for _, rec := range records {
				printfOut(cmd, "created %s (context %s, profile %s, %d layer(s))",
					rec.Name, rec.Context, rec.Profile, len(rec.Layers))
			}
			printfOut(cmd, "kubeconfig: kind-ocp kubeconfig <name> %s", scopeFlags(cfg))
			return nil
		},
	}
	return cmd
}

package cli

import (
	"github.com/spf13/cobra"
)

func newDeleteCmd(vals *flagValues, build clientBuilder) *cobra.Command {
	return &cobra.Command{
		Use:   "delete",
		Short: "Delete all clusters with the prefix and clear all state.",
		Long: `Delete every kind cluster whose name has the exact generated shape
<prefix>-<positive integer> — including clusters left over from a run with a
larger count — and remove the whole per-prefix state directory (kubeconfigs
and records). Clusters belonging to other prefixes are never touched.
Idempotent: absent clusters and absent state are not errors, and a later
create starts from zero state.`,
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
			deleted, err := client.Delete(cmd.Context())
			if err != nil {
				printfErr(cmd, "delete failed (re-run is safe): %v", err)
				return err
			}
			if len(deleted) == 0 {
				printfOut(cmd, "no clusters to delete (prefix %q)", cfg.Prefix)
			}
			for _, name := range deleted {
				printfOut(cmd, "deleted %s", name)
			}
			printfOut(cmd, "state cleared under %s", client.StateRoot())
			return nil
		},
	}
}

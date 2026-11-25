package cli

import (
	"github.com/spf13/cobra"
)

func newKubeconfigCmd(vals *flagValues, build clientBuilder) *cobra.Command {
	return &cobra.Command{
		Use:   "kubeconfig NAME",
		Short: "Print the path to a cluster's kubeconfig.",
		Long: `Print the path to the named cluster's kubeconfig in the state directory.
The kubeconfig is re-exported from the live cluster when the state copy is
missing.`,
		Args: cobra.ExactArgs(1),
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
			path, err := client.Kubeconfig(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			printfOut(cmd, "%s", path)
			return nil
		},
	}
}

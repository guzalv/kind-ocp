package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newListCmd(vals *flagValues, build clientBuilder) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List prefix-scoped clusters with state and applied layers.",
		Args:  cobra.NoArgs,
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
			infos, err := client.List(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 3, ' ', 0)
			render := func(format string, args ...any) {
				// Rendering errors on a broken pipe are not actionable; the
				// exit code is the failure signal.
				_, _ = fmt.Fprintf(w, format+"\n", args...)
			}
			render("NAME\tCONTEXT\tSTATE\tRECORD\tPROFILE\tLAYERS")
			for _, i := range infos {
				record := i.Status
				if !i.Managed {
					record = "unmanaged"
				}
				render("%s\t%s\t%s\t%s\t%s\t%s",
					i.Name, i.Context, i.State, record, i.Profile, joinLayers(i.Layers))
			}
			return w.Flush()
		},
	}
}

func joinLayers(layers []string) string {
	out := ""
	for i, l := range layers {
		if i > 0 {
			out += ","
		}
		out += l
	}
	return out
}

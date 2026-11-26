// Package cli wires the kind-ocp cobra commands on top of the kindocp
// façade. The commands are thin: resolve config, build a client, print the
// result. Output goes through cobra's per-command writers (OutOrStdout /
// ErrOrStderr), so embedded use and tests can capture it.
package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/guzalv/kind-ocp/config"
	"github.com/guzalv/kind-ocp/internal/shell"
	"github.com/guzalv/kind-ocp/kindocp"
)

// flagValues holds raw flag values shared across the command tree. The
// resolver only sees values whose flag was actually passed.
type flagValues struct {
	configPath string
	prefix     string
	stateRoot  string
	count      int
	profile    string
}

// clientBuilder produces the kindocp client for one command run. Tests
// substitute it; production uses the façade's shipped defaults.
type clientBuilder func(cfg *config.Config, log *slog.Logger) (*kindocp.Client, error)

// defaultClient builds the production client: the façade supplies the
// builtin layer registry and the kind provisioner.
func defaultClient(cfg *config.Config, log *slog.Logger) (*kindocp.Client, error) {
	return kindocp.New(kindocp.Options{Config: cfg, Log: log})
}

// Execute runs the kind-ocp CLI.
func Execute() error {
	return newRootCmd(defaultClient).Execute()
}

func newRootCmd(build clientBuilder) *cobra.Command {
	vals := &flagValues{}

	root := &cobra.Command{
		Use:   "kind-ocp",
		Short: "Create N OCP-like kind clusters.",
		Long: `kind-ocp creates N (default 2) kind clusters and applies a configurable
set of OpenShift layers to each, so StackRox Central/Sensor and the
Compliance Operator can be deployed and work properly.

Configuration precedence, highest first: flags, KIND_OCP_ environment
variables, the config file passed via --config, built-in defaults. Only the
winning value of each setting is parsed. An explicitly empty --config
(--config '') disables config-file lookup, KIND_OCP_CONFIG included.

--count and --profile select what create builds; the other commands do not
read their values, so a create command line can be reused verbatim. A
malformed flag value (for example --count=abc) is still rejected when the
flag is parsed, on every command.`,
		SilenceUsage: true,
	}

	pf := root.PersistentFlags()
	pf.StringVar(&vals.configPath, "config", "", "path to the config file (env KIND_OCP_CONFIG); an explicitly empty value disables file lookup")
	pf.StringVar(&vals.prefix, "prefix", "", "cluster name prefix; clusters are <prefix>-<i> (env KIND_OCP_PREFIX; default kind-ocp)")
	pf.StringVar(&vals.stateRoot, "state-root", "", "state root directory (default $XDG_STATE_HOME/kind-ocp)")
	pf.IntVar(&vals.count, "count", 0, "number of clusters to create (env KIND_OCP_COUNT; default 2; create only)")
	pf.StringVar(&vals.profile, "profile", "", "layer profile for create (env KIND_OCP_PROFILE; default full; create only)")

	root.AddCommand(
		newCreateCmd(vals, build),
		newDeleteCmd(vals, build),
		newListCmd(vals, build),
		newKubeconfigCmd(vals, build),
		newVerifyCmd(vals, build),
	)
	return root
}

// loadConfig resolves the configuration from flags and environment for the
// command being run.
//
// --count and --profile (and their environment counterparts) are read only
// for create: the other commands ignore them by design, and not reading
// them means a bad KIND_OCP_COUNT or a create-only mistake can never block
// cleanup. The config file is shared: it is still parsed strictly for every
// command, but its create-only values are validated in the create path
// only (config.Config.ValidateCreate).
func loadConfig(cmd *cobra.Command, vals *flagValues) (*config.Config, error) {
	flags := config.Flags{}
	if cmd.Flags().Changed("config") {
		flags.ConfigPath = &vals.configPath
	}
	if cmd.Flags().Changed("prefix") {
		flags.Prefix = &vals.prefix
	}
	if cmd.Flags().Changed("state-root") {
		flags.StateRoot = &vals.stateRoot
	}
	env := config.Env{
		ConfigPath: os.Getenv("KIND_OCP_CONFIG"),
		Prefix:     os.Getenv("KIND_OCP_PREFIX"),
	}
	if cmd.Name() == "create" {
		if cmd.Flags().Changed("count") {
			flags.Count = &vals.count
		}
		if cmd.Flags().Changed("profile") {
			flags.Profile = &vals.profile
		}
		env.Count = os.Getenv("KIND_OCP_COUNT")
		env.Profile = os.Getenv("KIND_OCP_PROFILE")
	}
	return config.Resolve(flags, env)
}

// newLogger builds the CLI logger on the command's error writer.
func newLogger(cmd *cobra.Command) *slog.Logger {
	return slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// scopeFlags renders the flags that make a printed kind-ocp command
// independent of the environment it is pasted into: --config ” disables
// config-file lookup (a stale or losing KIND_OCP_CONFIG in the paste
// environment must not break recovery), and the effective prefix and state
// root are pinned. Every value is POSIX-quoted by internal/shell so state
// roots containing spaces or quotes survive the copy-paste.
func scopeFlags(cfg *config.Config) string {
	return fmt.Sprintf("--config %s --prefix %s --state-root %s",
		shell.Quote(""), shell.Quote(cfg.Prefix), shell.Quote(cfg.StateRoot))
}

// deleteCommand renders the cleanup command for the effective prefix and
// state root. It must run verbatim in the same shell the failed create ran
// in, whatever KIND_OCP_CONFIG that shell carries.
func deleteCommand(cfg *config.Config) string {
	return "kind-ocp delete " + scopeFlags(cfg)
}

// printfErr writes a plain line to the command's error writer; used for
// hints that must be visible even when the structured log is quiet.
func printfErr(cmd *cobra.Command, format string, args ...any) {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...)
}

// printfOut writes a plain line to the command's output writer. Write
// errors (for example a closed pipe) are deliberately ignored: the
// command's exit code is the failure signal, not rendered output.
func printfOut(cmd *cobra.Command, format string, args ...any) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), format+"\n", args...)
}

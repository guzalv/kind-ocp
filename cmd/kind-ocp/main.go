// Command kind-ocp creates N OCP-like kind clusters.
package main

import (
	"os"

	"github.com/stackrox/kind-ocp/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}

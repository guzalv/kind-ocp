package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Built-in defaults. These are the only global defaults in the codebase
// (AC-4.2): every pinned external input used by the provisioning core lives
// here; each layer owns the defaults for its own settings. Everything is
// overridable through the config file.
const (
	// DefaultPrefix names clusters <prefix>-<i>; kubectl contexts follow
	// kind's rule and are kind-<name>.
	DefaultPrefix = "kind-ocp"

	// DefaultCount matches the product statement: N clusters, default 2.
	DefaultCount = 2

	// DefaultProfile enables every registered layer. With no builtin
	// layers registered this is equivalent to minimal.
	DefaultProfile = "full"

	// DefaultKindNodeImage is the kind v0.33 node image for Kubernetes
	// v1.36.4, digest-pinned (multi-arch: linux/amd64 and linux/arm64).
	// This is the workflow pin from the ported PoC; the PoC's script default
	// (v1.31.2, floating tag) must not be used.
	DefaultKindNodeImage = "kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed"

	defaultNodeReadyTimeout = 5 * time.Minute

	// maxCount is a safety cap: a typo like --count 100 would otherwise
	// attempt to boot 100 kind clusters on one Docker daemon.
	maxCount = 64
)

// DefaultStateRoot returns the default state root directory:
// $XDG_STATE_HOME/kind-ocp, falling back to ~/.local/state/kind-ocp.
func DefaultStateRoot() (string, error) {
	base, err := stateHome()
	if err != nil {
		return "", err
	}
	return base + "/kind-ocp", nil
}

// stateHome resolves the XDG state directory. kubeconfigs written under it
// are cluster-admin credentials, so callers must keep the tree 0700.
func stateHome() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving state home (set XDG_STATE_HOME or HOME): %w", err)
	}
	return filepath.Join(home, ".local", "state"), nil
}

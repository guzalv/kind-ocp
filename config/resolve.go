package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML accepts duration strings ("5m").
type Duration time.Duration

// UnmarshalYAML parses a duration string; an empty value leaves the duration
// unset (zero).
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	s := strings.TrimSpace(node.Value)
	if s == "" {
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Flags carries explicitly-set command-line flags. Nil pointers mean the
// flag was not passed; the distinction matters for precedence.
type Flags struct {
	Prefix    *string
	Count     *int
	Profile   *string
	StateRoot *string
	// ConfigPath is the --config flag. A non-nil pointer to an empty string
	// means --config was passed as empty ("--config ''"): config-file
	// lookup is disabled, KIND_OCP_CONFIG included.
	ConfigPath *string
}

// Env carries kind-ocp environment variables. Empty strings mean unset.
type Env struct {
	Prefix     string
	Count      string
	Profile    string
	ConfigPath string
}

var prefixPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidatePrefix checks that prefix can start generated cluster names. It is
// enforced both by Resolve and by consumers (the kindocp façade) that receive
// a Config directly, so an unvalidated prefix can never reach a filesystem
// join.
func ValidatePrefix(prefix string) error {
	if !prefixPattern.MatchString(prefix) || len(prefix) > 40 {
		return fmt.Errorf("prefix %q is not a valid cluster name prefix (lowercase alphanumeric and '-', 1-40 chars)", prefix)
	}
	return nil
}

// LoadFile reads and strictly decodes a config file. Unknown keys are errors:
// a typo like "profle:" must not silently fall back to a default. A file with
// more than one YAML document is rejected rather than silently truncating.
func LoadFile(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config file %s: more than one YAML document", path)
	}
	return &f, nil
}

// Resolve merges defaults, the config file (if any), environment variables,
// and flags into a Config. See the package comment for precedence.
//
// Resolve validates only what every command shares: the file's structural
// format and the prefix (the scope). Create-only semantics — count, profile,
// node image, timeouts, cluster overrides — are validated by
// Config.ValidateCreate, so a config with a create-only mistake still
// resolves for the scope commands (delete, list, verify, kubeconfig), which
// accept and ignore create inputs.
func Resolve(flags Flags, env Env) (*Config, error) {
	var file *File
	// An explicitly passed --config wins outright, and an explicitly empty
	// one ("--config ''") disables file lookup entirely — KIND_OCP_CONFIG
	// included. That is what makes commands kind-ocp prints for humans to
	// paste independent of the environment they are pasted into.
	path := env.ConfigPath
	if flags.ConfigPath != nil {
		path = *flags.ConfigPath
	}
	if path != "" {
		var err error
		file, err = LoadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config file %s: %w", path, err)
		}
	}

	cfg := &Config{
		Prefix:        DefaultPrefix,
		Count:         DefaultCount,
		Profile:       DefaultProfile,
		KindNodeImage: DefaultKindNodeImage,
		Timeouts:      TimeoutsConfig{NodeReady: defaultNodeReadyTimeout},
		Clusters:      map[string]ClusterOverride{},
		Profiles:      map[string]Profile{},
		Layers:        map[string]yaml.Node{},
	}

	if file != nil {
		overlayFile(cfg, file)
	}
	if err := overlayEnv(cfg, env, flags); err != nil {
		return nil, err
	}
	overlayFlags(cfg, flags)

	if cfg.StateRoot == "" {
		root, err := DefaultStateRoot()
		if err != nil {
			return nil, err
		}
		cfg.StateRoot = root
	}
	if abs, err := filepath.Abs(cfg.StateRoot); err == nil {
		cfg.StateRoot = abs
	}

	// Scope-only validation: the prefix must be safe for every command.
	// Create-only semantics (count, profile, node image, timeouts,
	// overrides) are checked by Config.ValidateCreate in the create path,
	// so a create-only mistake never blocks delete, list, verify, or
	// kubeconfig.
	if err := ValidatePrefix(cfg.Prefix); err != nil {
		return nil, err
	}
	return cfg, nil
}

func overlayFile(cfg *Config, file *File) {
	if file.Prefix != "" {
		cfg.Prefix = file.Prefix
	}
	if file.Count != nil {
		cfg.Count = *file.Count
	}
	if file.Profile != "" {
		cfg.Profile = file.Profile
	}
	if file.KindNodeImage != "" {
		cfg.KindNodeImage = file.KindNodeImage
	}
	if file.StateRoot != "" {
		cfg.StateRoot = file.StateRoot
	}
	if file.Timeouts.NodeReady != 0 {
		cfg.Timeouts.NodeReady = time.Duration(file.Timeouts.NodeReady)
	}
	for k, v := range file.Clusters {
		cfg.Clusters[k] = v
	}
	for k, v := range file.Profiles {
		cfg.Profiles[k] = v
	}
	for k, v := range file.Layers {
		cfg.Layers[k] = v
	}
}

// overlayEnv applies environment values that win over the file. Values whose
// flag counterpart was passed are skipped entirely: only the winning value of
// a key is parsed, so a malformed KIND_OCP_COUNT never fails a run that
// passes a valid --count.
func overlayEnv(cfg *Config, env Env, flags Flags) error {
	if env.Prefix != "" && flags.Prefix == nil {
		cfg.Prefix = env.Prefix
	}
	if env.Count != "" && flags.Count == nil {
		n, err := strconv.Atoi(env.Count)
		if err != nil {
			return fmt.Errorf("KIND_OCP_COUNT=%q is not a number", env.Count)
		}
		cfg.Count = n
	}
	if env.Profile != "" && flags.Profile == nil {
		cfg.Profile = env.Profile
	}
	return nil
}

func overlayFlags(cfg *Config, flags Flags) {
	if flags.Prefix != nil {
		cfg.Prefix = *flags.Prefix
	}
	if flags.Count != nil {
		cfg.Count = *flags.Count
	}
	if flags.Profile != nil {
		cfg.Profile = *flags.Profile
	}
	if flags.StateRoot != nil {
		cfg.StateRoot = *flags.StateRoot
	}
}

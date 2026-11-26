package layers

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/guzalv/kind-ocp/config"
)

type fakeLayer struct {
	name string
}

func (f fakeLayer) Name() string                          { return f.name }
func (f fakeLayer) Apply(_ context.Context, _ Env) error  { return nil }
func (f fakeLayer) Verify(_ context.Context, _ Env) error { return nil }

func newRegistry(t *testing.T, names ...string) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, n := range names {
		if err := reg.Register(fakeLayer{name: n}); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(fakeLayer{name: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(fakeLayer{name: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(fakeLayer{name: "a"}); err == nil {
		t.Fatal("expected duplicate-registration error")
	}
	if got, want := reg.Names(), []string{"b", "a"}; len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Errorf("Names() = %v, want %v (registration order)", got, want)
	}
	if _, ok := reg.Get("missing"); ok {
		t.Error("Get(missing) reported found")
	}
	if _, ok := reg.Get("a"); !ok {
		t.Error("Get(a) reported missing")
	}
}

// Layer names become cache directories under the state root
// (state.Store.CacheDirFor). The registry distinguishes layers by exact
// string, so two names differing only by case would be two layers
// sharing one directory on a case-insensitive filesystem (macOS
// resolves ACME-cache and acme-cache to the same directory). The
// registry must refuse such names at registration — before any layer
// runs — and must not store a layer it refused.
func TestRegistryRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{
		"ACME-cache", // case variant of a valid name: the alias itself
		"Acme-Cache", // mixed case
		"acme_cache", // underscore
		"acme.cache", // dot
		"acme/cache", // path separator
		"acmé",       // non-ASCII
		"",           // nothing
	} {
		reg := NewRegistry()
		if err := reg.Register(fakeLayer{name: name}); err == nil {
			t.Errorf("Register(%q) succeeded, want a safe-name rejection", name)
		}
		if _, ok := reg.Get(name); ok {
			t.Errorf("Register(%q) stored the layer despite refusing the name", name)
		}
		if names := reg.Names(); len(names) != 0 {
			t.Errorf("Register(%q) left %v registered", name, names)
		}
	}
}

func TestLayerNamesForProfile(t *testing.T) {
	reg := newRegistry(t, "one", "two", "three")

	t.Run("full is every registered layer in order", func(t *testing.T) {
		got, err := LayerNamesForProfile(reg, ProfileFull, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"one", "two", "three"}
		if len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("minimal is empty", func(t *testing.T) {
		got, err := LayerNamesForProfile(reg, ProfileMinimal, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})

	t.Run("custom profile applies in registration order", func(t *testing.T) {
		custom := map[string]config.Profile{
			"p": {Layers: []string{"three", "one"}},
		}
		got, err := LayerNamesForProfile(reg, "p", custom)
		if err != nil {
			t.Fatal(err)
		}
		// Listed order is three,one — applied in registration order one,three.
		if len(got) != 2 || got[0] != "one" || got[1] != "three" {
			t.Errorf("got %v, want [one three] (registration order)", got)
		}
	})

	t.Run("custom profile with unknown layer fails", func(t *testing.T) {
		custom := map[string]config.Profile{
			"p": {Layers: []string{"one", "nope"}},
		}
		if _, err := LayerNamesForProfile(reg, "p", custom); err == nil {
			t.Fatal("expected unknown-layer error")
		}
	})

	t.Run("custom profile with duplicate layer fails", func(t *testing.T) {
		custom := map[string]config.Profile{
			"p": {Layers: []string{"one", "one"}},
		}
		if _, err := LayerNamesForProfile(reg, "p", custom); err == nil {
			t.Fatal("expected duplicate-layer error")
		}
	})

	t.Run("unknown profile fails", func(t *testing.T) {
		if _, err := LayerNamesForProfile(reg, "nope", nil); err == nil {
			t.Fatal("expected unknown-profile error")
		}
	})
}

func TestEnvDecodeSettings(t *testing.T) {
	type settings struct {
		Version string   `yaml:"version"`
		Names   []string `yaml:"names"`
	}

	parseNode := func(t *testing.T, doc string) map[string]yaml.Node {
		t.Helper()
		var raw map[string]yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}

	t.Run("missing section keeps defaults", func(t *testing.T) {
		env := Env{Settings: map[string]yaml.Node{}}
		s := settings{Version: "default"}
		if err := env.DecodeSettings("olm", &s); err != nil {
			t.Fatal(err)
		}
		if s.Version != "default" {
			t.Errorf("version = %q, want default preserved", s.Version)
		}
	})

	t.Run("null section keeps defaults", func(t *testing.T) {
		env := Env{Settings: parseNode(t, "olm:\n")}
		s := settings{Version: "default"}
		if err := env.DecodeSettings("olm", &s); err != nil {
			t.Fatal(err)
		}
		if s.Version != "default" {
			t.Errorf("version = %q, want default preserved", s.Version)
		}
	})

	t.Run("present keys override defaults", func(t *testing.T) {
		env := Env{Settings: parseNode(t, "olm:\n  version: v1\n  names: [a, b]\n")}
		s := settings{Version: "default", Names: []string{"x"}}
		if err := env.DecodeSettings("olm", &s); err != nil {
			t.Fatal(err)
		}
		if s.Version != "v1" {
			t.Errorf("version = %q, want v1", s.Version)
		}
		if len(s.Names) != 2 || s.Names[0] != "a" || s.Names[1] != "b" {
			t.Errorf("names = %v, want [a b]", s.Names)
		}
	})

	t.Run("unknown key fails", func(t *testing.T) {
		env := Env{Settings: parseNode(t, "olm:\n  versio: v1\n")}
		s := settings{}
		if err := env.DecodeSettings("olm", &s); err == nil {
			t.Fatal("expected unknown-key error (strict decoding)")
		}
	})
}

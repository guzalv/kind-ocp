package shell

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"/Users/x/.local/state/kind-ocp", "/Users/x/.local/state/kind-ocp"},
		{"kind-ocp-1", "kind-ocp-1"},
		{"/tmp/my state", `'/tmp/my state'`},
		{"it's", `'it'\''s'`},
		{"", "''"},
		{"$HOME", "'$HOME'"},
		{"`x`", "'`x`'"},
		{"a\"b", `'a"b'`},
		{"a;rm -rf /", `'a;rm -rf /'`},
		{"*", "'*'"},
		{"hello world's end", `'hello world'\''s end'`},
	}
	for _, tc := range cases {
		if got := Quote(tc.in); got != tc.want {
			t.Errorf("Quote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The contract is paste-ability: whatever Quote renders must reassemble to
// the exact original word when a real POSIX shell parses it. This test asks
// an actual sh to do the parsing rather than trusting the string shape.
func TestQuoteRoundTripsThroughShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX sh available")
	}
	words := []string{
		"plain",
		"/Users/x/.local/state/kind-ocp",
		"/tmp/my state/root",
		"it's",
		"",
		"a b'c\"d",
		"$HOME `x`",
		"* ? [a-z]",
		"semi;colon|pipe&amp",
	}
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = Quote(w)
	}
	// printf prints one word per line; none of the test words contain
	// newlines, so the split is unambiguous.
	cmd := exec.Command("sh", "-c", "printf '%s\\n' "+strings.Join(quoted, " "))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sh failed to parse the quoted words: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if !reflect.DeepEqual(got, words) {
		t.Errorf("shell reassembled %q, want the original %q", got, words)
	}
}

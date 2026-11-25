// Package shell renders command lines that kind-ocp prints for humans to
// copy and paste. The one rule: a rendered argument must reassemble to the
// exact original word under any POSIX shell, whatever characters it
// contains.
package shell

import (
	"regexp"
	"strings"
)

// safeWord matches the characters that carry no meaning to a POSIX shell in
// an unquoted word: a value consisting solely of these is already safe.
var safeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// Quote renders s as a single POSIX shell word. Words made only of
// shell-safe characters are returned unchanged; anything else (spaces,
// quotes, expansions, globs) is single-quoted with embedded single quotes
// escaped the standard way ('...'...). The empty string becomes ” — an
// empty unquoted word would vanish. The result parses back to exactly s.
func Quote(s string) string {
	if s != "" && safeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

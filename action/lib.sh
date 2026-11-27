# action/lib.sh — shared helpers for the kind-ocp consumer actions
# (docs/PLAN.md §8). Sourced, never executed: every run step that
# validates an input or runs a tool carries an ACTION_LIB env
# mapping pointing here (the runner-resolved action path plus
# /../lib.sh) and does `source "$ACTION_LIB"`, so the path is an
# environment value like every input, never script language. One
# implementation instead of a copy per step per action.
#
# Threat model. The actions' inputs are caller-controlled bytes,
# and the tools they drive echo input fragments verbatim in their
# own diagnostics: kind-ocp renders the config path and the state
# root unquoted, go renders the module@version operand unquoted,
# and kind-ocp's config parse errors can quote config-file CONTENT
# — which no input rule reaches. The runner parses workflow
# commands out of the step log in two formats (actions/runner
# v2.338.2, src/Runner.Common/ActionCommand.cs and
# src/Runner.Worker/Handlers/OutputManager.cs): the modern
# '::name::' introducer at the start of a line, after leading
# whitespace (the parser TrimStarts), and the legacy '##[name]'
# introducer ANYWHERE on a line. A bare CR ends a log line (the
# runner reads process output with .NET line semantics). A
# single-line input is therefore not a safe input, and %q quoting
# is not a defense: an escaped-newline value arrives as one
# physical line, which is exactly the shape the legacy parser
# accepts mid-line. Every log-bound tool byte below is made inert
# instead; the input refusals stay as defense in depth.
#
# Exit statuses are preserved: ko_run_tool returns the tool's own
# status (PIPESTATUS), ko_capture_tool replays captured stderr
# after the tool exits. Both are errexit-safe — a bare call under
# `set -euo pipefail` fails the step with the tool's status, and a
# checked call (`|| …`, `if ! …`) runs with errexit suppressed
# inside, so the caller sees the status and decides.

# ko_neutralize_log — stdin to stdout, both workflow-command
# introducers made inert:
#   - CR becomes LF first, so whatever follows a bare CR is
#     neutralized as its own line (sed sees the split);
#   - '##[' becomes '##\[' globally — the legacy introducer is
#     parsed anywhere on a line, so the sequence must not survive
#     (the backslash insertion is the same transformation the
#     runner's own OutputManager applies to its internal markers);
#   - a line-leading '::' (after leading whitespace, which the
#     modern parser TrimStarts) becomes ': :'.
ko_neutralize_log() {
  tr '\r' '\n' | sed -e 's/##\[/##\\[/g' -e 's/^\([[:space:]]*\)::/\1: :/'
}

# ko_run_tool CMD... — run a tool whose stdout and stderr are
# log-bound: both are sanitized on their way to the runner log,
# and the tool's exit status is returned.
ko_run_tool() {
  "$@" 2>&1 | ko_neutralize_log
  local status="${PIPESTATUS[0]}"
  return "$status"
}

# ko_capture_tool CMD... — run a tool whose stdout is DATA for the
# caller (a command substitution), so stdout stays raw; stderr is
# still log-bound and is replayed sanitized after the tool exits.
# Returns the tool's exit status.
ko_capture_tool() {
  local err status=0
  err="$(mktemp)"
  "$@" 2>"$err" || status=$?
  ko_neutralize_log <"$err" >&2 || true
  rm -f "$err"
  return "$status"
}

# ko_require_single_line LABEL VALUE — refuse a value carrying CR
# or LF, with a fixed message that never echoes the value: the
# runner parses workflow commands from stderr too.
ko_require_single_line() {
  case "$2" in
    *$'\r'*|*$'\n'*)
      echo "$1 must be a single line (no CR or LF)" >&2
      return 1 ;;
  esac
}

# ko_require_version_token LABEL VALUE — refuse a version-spec
# value that is not one token of [A-Za-z0-9._~^+<>/=-]. go_version
# crosses into actions/setup-go through `with:` and setup-go logs
# the spec verbatim in its own step (`Setup go version spec …`,
# setup-go v7.1.0 src/main.ts) — output these actions cannot
# sanitize — so the charset is the defense: without ':', '#', '[',
# quotes, or whitespace the value cannot form either introducer.
# kind_ocp_version gets the same rule (go renders the module@version
# operand unquoted). CR and LF are subsumed: neither is in the
# charset. Fixed message, never the raw value. The pattern lives in
# a variable because bash tokenizes '<' and '>' inside a case
# pattern as redirections.
ko_require_version_token() {
  local bad='*[!A-Za-z0-9._~^+<>/=-]*'
  case "$2" in
    '')
      echo "$1 must not be empty" >&2
      return 1 ;;
    $bad)
      echo "$1 may contain only letters, digits, and . _ ~ ^ + < > / = - (no CR, LF, ':', '#', '[', quotes, or whitespace)" >&2
      return 1 ;;
  esac
}

#!/usr/bin/env bash
# local-gate.sh — gates the consumer actions by running their own
# `run:` scripts, called by name, with test values supplied
# explicitly, against the local Docker daemon. NOT part of CI: the
# remote self-test (self-test.yaml, moved to
# .github/workflows/action-self-test.yaml by MOVE.md) is the
# operative Linux-runner proof; this gate covers what a macOS +
# Docker Desktop machine can prove (docs/PLAN.md §8).
#
# Design: the gate is NOT a runner emulator. It never resolves a
# ${{ }} expression and never builds a runner context. It records,
# for every run step of both actions, the EXACT wiring it expects
# that step to declare — id, shell, and the whole env mapping,
# verbatim — and asserts each action.yml carries exactly that
# wiring before anything runs (a purely static check: it fails with
# empty default inputs and nothing supplied). It then runs each
# named step's own script with test values supplied explicitly for
# exactly those env keys. Step-to-step data flow — the scope step's
# outputs feeding the later steps — is read from the scope step's
# own GITHUB_OUTPUT file and passed on explicitly, which is what
# the asserted `${{ steps.scope.outputs.* }}` env wiring delivers
# on a real runner. Inputs a scenario does not pin fall back to the
# action.yml's declared default, read straight from the YAML.
#
# Run steps carrying `if:`, `continue-on-error:`, or
# `working-directory:` are refused outright: the gate executes
# every step unconditionally in its own scratch directory and takes
# every failure, so a condition, override, or working directory it
# would silently ignore must fail statically instead.
#
# From the runner interface it takes only: per-step GITHUB_OUTPUT
# files, one cumulative GITHUB_PATH per action run, GITHUB_RUN_ID /
# RUNNER_TEMP. The `uses:` steps are skipped (Go is provided by the
# harness instead of actions/setup-go). Ambient KIND_OCP_* and GOBIN
# values are planted around every step: the scripts must neutralize
# the first and override the second.
#
# The step arrays (CREATE_RUN_STEPS / DELETE_RUN_STEPS) are the
# single source of the gate's call sequence: the yaml scenario
# asserts each array equals the action.yml's declared run steps,
# and the drivers iterate the arrays, so a step the action declares
# is either missing from its array (yaml fails) or has no driver
# branch (the run dies) — it cannot silently never run.
#
# Scenarios (argument; `all` runs them in order):
#   yaml    A: staged YAML parses; remote pins resolve on GitHub;
#             injection, pin-shape, and step-sequence invariants
#             hold — over the two actions and the self-test
#             explicitly, so nothing is lost when MOVE.md relocates
#             them
#   smoke   A: exact per-step wiring (id, shell, env mapping, no
#             conditions or overrides) and the action outputs
#             mapping; supplied and default inputs behave, and the
#             default prefix is unique per invocation under one
#             run id; a CR/LF state_root, a multiline prefix, a
#             multiline config path, a multiline kind-ocp version,
#             and a multiline delete-side prefix or state_root are
#             refused without the raw value reaching the log;
#             delete enforces its required prefix
#   e2e     B+D: a default-input create of 2 full-profile clusters
#             through the create action's own scripts, the output
#             contract, verify-failure propagation, delete, clean
#   failure C: a create killed mid-run (its own process group only)
#             still exposed prefix/state_root; a self-sufficient
#             delete cleans the half-created prefix
#   spaced  D: a state root containing a space keeps the clusters
#             JSON valid and usable
#
# Needs on PATH: bash, go, docker, jq, yq (mikefarah), kubectl,
# and curl (pin resolution only). Each run gets a fresh mktemp -d
# scratch root and a unique run id (so the derived
# ko-<run id>-<random> prefixes never collide with another run's
# or invocation's scope); the root is removed on success and kept
# for inspection on failure.

set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"

# The actions live under action/ while staged and under
# .github/actions/ after the MOVE.md handoff; support both so this
# gate survives the move unchanged.
ACTIONS_DIR="$REPO/action"
SELF_TEST_YML="$ACTIONS_DIR/self-test.yaml"
if [ ! -d "$ACTIONS_DIR/create" ]; then
  ACTIONS_DIR="$REPO/.github/actions"
  SELF_TEST_YML="$REPO/.github/workflows/action-self-test.yaml"
fi
CREATE_YML="$ACTIONS_DIR/create/action.yml"
DELETE_YML="$ACTIONS_DIR/delete/action.yml"

die() {
  echo "FAIL: $*" >&2
  echo "gate root kept for inspection: $GATE_ROOT" >&2
  exit 1
}
pass() { echo "PASS: $*"; }

# Fresh scratch space, always self-created (never a caller-selected
# root) and removed only by this run's success path; unique run id
# per gate run.
GATE_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/kind-ocp-gate.XXXXXXXX")"
mkdir -p "$GATE_ROOT/ambient-gobin"
RUN_ID="$$${RANDOM}"

# The PATH every step starts from: the tools the steps themselves
# need, and deliberately NOT $(go env GOPATH)/bin — an install
# step's GITHUB_PATH write is the only thing that puts kind-ocp on
# it, which is exactly what the runner does.
GATE_PATH_BASE="/usr/bin:/bin:/usr/sbin:/sbin"
for t in go docker jq yq kubectl curl; do
  command -v "$t" >/dev/null 2>&1 || die "required tool not on PATH: $t"
  GATE_PATH_BASE="$(dirname "$(command -v "$t")"):$GATE_PATH_BASE"
done

# DOCKER_HOST is pinned per docs/PLAN.md §5.1 when unset and the
# Docker Desktop socket exists.
export DOCKER_HOST="${DOCKER_HOST:-}"
if [ -z "$DOCKER_HOST" ] && [ -S "$HOME/.docker/run/docker.sock" ]; then
  export DOCKER_HOST="unix://$HOME/.docker/run/docker.sock"
fi

# The only expression forms the gate's recorded expectations carry
# (checked against the declared inputs and step ids — never
# resolved).
P_INPUTS='${{ inputs.'
P_STEPS='${{ steps.'
P_END=' }}'

# The GITHUB_RUN_ID of the current action run.
CURRENT_RUN_ID="$RUN_ID"

# The gate's step sequences: every run step of each action, by
# name, in order — the single source of the call sequence. The
# yaml scenario asserts each array equals the action.yml's run
# steps; the drivers iterate the arrays.
CREATE_RUN_STEPS=(
  "Resolve the scope"
  "Install kind-ocp"
  "Create the clusters"
  "Emit the clusters output"
  "Verify the clusters"
)
DELETE_RUN_STEPS=(
  "Install kind-ocp"
  "Delete the clusters"
)

# ------------------------------------------------- recorded expectations

# expected_step_id ACTION STEP — the id the named run step must
# declare ('': none). The later steps' env and the create action's
# outputs reference steps.scope / steps.clusters, so the ids are
# part of the wiring the gate asserts.
expected_step_id() {
  case "$1/$2" in
    create/"Resolve the scope") echo "scope" ;;
    create/"Emit the clusters output") echo "clusters" ;;
    *) echo "" ;;
  esac
}

# expected_step_env ACTION STEP — the exact env mapping the named
# run step must declare: one KEY=VALUE line per entry, the VALUE
# verbatim from the action.yml. The gate asserts the actions carry
# exactly this wiring and supplies test values for exactly these
# keys; no expression is ever resolved.
expected_step_env() {
  case "$1/$2" in
    create/"Resolve the scope")
      printf '%s\n' \
        'INPUT_PREFIX=${{ inputs.prefix }}' \
        'INPUT_STATE_ROOT=${{ inputs.state_root }}'
      ;;
    create/"Install kind-ocp" | delete/"Install kind-ocp")
      printf '%s\n' \
        'INPUT_KIND_OCP_VERSION=${{ inputs.kind_ocp_version }}'
      ;;
    create/"Create the clusters")
      printf '%s\n' \
        'INPUT_COUNT=${{ inputs.count }}' \
        'INPUT_PROFILE=${{ inputs.profile }}' \
        'INPUT_CONFIG=${{ inputs.config }}' \
        'KIND_OCP_ACTION_PREFIX=${{ steps.scope.outputs.prefix }}' \
        'KIND_OCP_ACTION_STATE_ROOT=${{ steps.scope.outputs.state_root }}'
      ;;
    create/"Emit the clusters output" | create/"Verify the clusters")
      printf '%s\n' \
        'INPUT_CONFIG=${{ inputs.config }}' \
        'KIND_OCP_ACTION_PREFIX=${{ steps.scope.outputs.prefix }}' \
        'KIND_OCP_ACTION_STATE_ROOT=${{ steps.scope.outputs.state_root }}'
      ;;
    delete/"Delete the clusters")
      printf '%s\n' \
        'INPUT_PREFIX=${{ inputs.prefix }}' \
        'INPUT_STATE_ROOT=${{ inputs.state_root }}'
      ;;
    *)
      die "no recorded expectation for $1 step '$2' — the gate's recorded wiring is stale"
      ;;
  esac
}

# expected_action_outputs ACTION — the exact outputs section the
# action must declare (KEY=VALUE lines, the VALUE verbatim); the
# delete action declares none.
expected_action_outputs() {
  case "$1" in
    create)
      printf '%s\n' \
        'prefix=${{ steps.scope.outputs.prefix }}' \
        'state_root=${{ steps.scope.outputs.state_root }}' \
        'clusters=${{ steps.clusters.outputs.clusters }}'
      ;;
    delete) : ;;
  esac
}

# --------------------------------------------------------- static checks

# check_step ACTION STEP_JSON YML — asserts ONE run step carries
# exactly the gate's recorded wiring, before anything runs (it
# fails with empty default inputs and nothing supplied):
#   - shell: bash exactly;
#   - no `if:` condition, no `continue-on-error:` override, no
#     `working-directory:` — the gate runs every step
#     unconditionally in its own scratch directory and takes every
#     failure, so anything it would silently ignore fails here;
#   - the step's id and its whole env mapping equal the recorded
#     expectation, verbatim (YAML mappings are unordered, so both
#     sides are sorted before the compare);
#   - every INPUT_* the script references is a key of the recorded
#     env — an unmapped reference would be an unset variable on a
#     real runner (composite run steps get no automatic INPUT_*
#     variables; GitHub documents this);
#   - every `${{ inputs.<name> }}` / `${{ steps.<id>.… }}` value in
#     the recorded env names a declared input / step id of that
#     action — a renamed input or id would leave the reference
#     empty on a real runner while still passing a literal compare.
check_step() {
  local action="$1" step_json="$2" yml="$3"
  local name shell id expected got env_keys ref val iname sid
  local declared_inputs declared_ids
  name="$(jq -r '.name // "(unnamed)"' <<<"$step_json")"
  shell="$(jq -r '.shell // ""' <<<"$step_json")"
  [ "$shell" = "bash" ] \
    || die "$yml: step '$name' must declare shell: bash (got '${shell:-none}') — the gate runs scripts through bash"
  if jq -e 'has("if")' <<<"$step_json" >/dev/null; then
    die "$yml: step '$name' carries an if: condition — the gate runs every step unconditionally and would ignore it"
  fi
  if jq -e 'has("continue-on-error")' <<<"$step_json" >/dev/null; then
    die "$yml: step '$name' carries continue-on-error: — the gate takes every failure and would ignore the override"
  fi
  if jq -e 'has("working-directory")' <<<"$step_json" >/dev/null; then
    die "$yml: step '$name' carries working-directory: — the gate runs every step in its own scratch directory and would ignore it"
  fi
  id="$(jq -r '.id // ""' <<<"$step_json")"
  expected="$(expected_step_id "$action" "$name")"
  [ "$id" = "$expected" ] \
    || die "$yml: step '$name' declares id '${id:-none}', expected '${expected:-none}'"
  expected="$(expected_step_env "$action" "$name" | LC_ALL=C sort)"
  got="$(jq -r '(.env // {}) | to_entries[] | "\(.key)=\(.value)"' <<<"$step_json" | LC_ALL=C sort)"
  [ "$expected" = "$got" ] \
    || die "$yml: step '$name' env mapping differs from the gate's recorded expectation:
--- recorded ---
$expected
--- declared ---
$got"
  env_keys="$(expected_step_env "$action" "$name" | sed 's/=.*//')"
  while read -r ref; do
    [ -n "$ref" ] || continue
    grep -Fxq "$ref" <<<"$env_keys" \
      || die "$yml: step '$name' references \$$ref, but its recorded env maps nothing onto it — composite run steps get no automatic INPUT_* variables, so the script would never see it on a real runner"
  done < <(jq -r '.run' <<<"$step_json" | grep -oE 'INPUT_[A-Z0-9_]+' | sort -u || true)
  declared_inputs="$(yq -r '.inputs // {} | keys | join("\n")' "$yml")"
  declared_ids="$(yq -r '[.runs.steps[] | select(.id != null) | .id] | join("\n")' "$yml")"
  while read -r val; do
    [ -n "$val" ] || continue
    case "$val" in
      "$P_INPUTS"*"$P_END")
        iname="${val#"$P_INPUTS"}"; iname="${iname%"$P_END"}"
        grep -Fxq "$iname" <<<"$declared_inputs" \
          || die "$yml: the recorded env maps a value from inputs.$iname, which is not a declared input"
        ;;
      "$P_STEPS"*.outputs.*"$P_END")
        sid="${val#"$P_STEPS"}"; sid="${sid%%.outputs.*}"
        grep -Fxq "$sid" <<<"$declared_ids" \
          || die "$yml: the recorded env references $val, but no step declares the id '$sid'"
        ;;
      *)
        die "internal: recorded env value is neither an inputs nor a steps reference: $val"
        ;;
    esac
  done < <(expected_step_env "$action" "$name" | sed 's/^[^=]*=//')
}

# check_action_outputs ACTION YML — the action's outputs section
# equals the gate's recorded expectation, verbatim: this is the
# mapping a consumer's steps.<id>.outputs.<name> reads, so a wrong
# step id or key here breaks consumers while every step still runs
# green.
check_action_outputs() {
  local expected got
  expected="$(expected_action_outputs "$1" | LC_ALL=C sort)"
  got="$(yq -r '(.outputs // {}) | to_entries[] | .key + "=" + .value.value' "$2" | LC_ALL=C sort)"
  [ "$expected" = "$got" ] \
    || die "$2: the outputs section differs from the gate's recorded expectation:
--- recorded ---
$expected
--- declared ---
$got"
}

# ------------------------------------------------------------ execution

# github_output_get FILE KEY — prints KEY's value from a
# GITHUB_OUTPUT file. The actions emit single-line key=value
# directives only (their scripts refuse CR/LF inputs), so this
# parser is deliberately that simple.
github_output_get() {
  awk -F= -v want="$2" '
    index($0, "=") > 1 && substr($0, 1, index($0, "=") - 1) == want {
      print substr($0, index($0, "=") + 1); exit
    }
  ' "$1"
}

# gate_path WORK — the PATH a step runs with: GATE_PATH_BASE plus
# everything earlier steps appended to WORK/github-path (prepended,
# the way the runner applies it).
gate_path() {
  local p="$GATE_PATH_BASE" line
  if [ -f "$1/github-path" ]; then
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      case ":$p:" in
        *":$line:"*) ;;
        *) p="$line:$p" ;;
      esac
    done <"$1/github-path"
  fi
  printf '%s\n' "$p"
}

# fresh_work DIR — a fresh action-run context: empty GITHUB_PATH,
# empty step-output directory, new RUNNER_TEMP. Called only on
# subpaths of this run's GATE_ROOT.
fresh_work() {
  rm -rf "$1"
  mkdir -p "$1/logs" "$1/runner-temp" "$1/step-outputs"
  : >"$1/github-path"
}

# declared_default YML NAME — the action.yml's declared default for
# NAME ('' when it declares none), read the way a runner feeds an
# input a consumer's `with:` leaves unset.
declared_default() {
  yq -r ".inputs.\"$2\".default // \"\"" "$1"
}

# with_input YML NAME [WITH...] — the value a consumer's `with:`
# sets for NAME: the override when one is given, else the declared
# default.
with_input() {
  local yml="$1" name="$2" ov
  shift 2
  for ov in "$@"; do
    if [ "${ov%%=*}" = "$name" ]; then printf '%s' "${ov#*=}"; return 0; fi
  done
  declared_default "$yml" "$name"
}

# run_step ACTION NAME WORK TAG ENV... — runs ONE named run step's
# own script under the gate's step environment. check_step's static
# assertions run first; the script then runs with exactly the
# caller-supplied env values, which must cover exactly the recorded
# key set (no extras, none missing) — the gate supplies test values,
# it never resolves an expression. TAG keeps re-runs of the same
# step from overwriting the first log. Returns the step's exit
# status.
run_step() {
  local action="$1" want="$2" work="$3" tag="$4"
  shift 4
  local yml step script id out log rc=0 kv
  local expected_keys supplied_keys
  yml="$ACTIONS_DIR/$action/action.yml"
  # Name selection: ONE yq query by name — strenv reads GATE_STEP
  # from this command's environment. Wrapping the matches in an
  # array yields a single JSON document, and requiring EXACTLY one
  # match makes a duplicated step name fail here instead of
  # silently running whichever answered first.
  step="$(GATE_STEP="$want" yq -o=json \
    '[.runs.steps[] | select(.name == strenv(GATE_STEP))]' "$yml")" \
    || die "cannot read the steps of $yml"
  [ "$(jq 'length' <<<"$step")" -eq 1 ] \
    || die "$yml: expected exactly one step named '$want' (found $(jq 'length' <<<"$step"))"
  step="$(jq -c '.[0]' <<<"$step")"
  check_step "$action" "$step" "$yml"
  script="$(jq -r '.run' <<<"$step")"
  id="$(jq -r '.id // ""' <<<"$step")"
  out="$work/step-outputs/${id:-$(printf '%s' "$want" | tr ' /' '__')}.output"
  # The supplied values must cover exactly the recorded key set.
  # Keys are taken per argument (values may span lines — the CR/LF
  # probe supplies one), so a missing or extra mapping is a gate-side
  # bug, not a silent empty.
  expected_keys="$(expected_step_env "$action" "$want" | sed 's/=.*//' | LC_ALL=C sort)"
  supplied_keys="$(for kv in "$@"; do printf '%s\n' "${kv%%=*}"; done | LC_ALL=C sort)"
  [ "$expected_keys" = "$supplied_keys" ] \
    || die "run_step '$want': the supplied values must cover exactly the recorded env keys
--- recorded ---
$expected_keys
--- supplied ---
$supplied_keys"
  : >"$out"
  log="$work/logs/$tag$(printf '%s' "$want" | tr ' /' '__').log"
  echo "--- step: $want"
  (
    for kv in "$@"; do
      export "${kv%%=*}=${kv#*=}"
    done
    export GITHUB_OUTPUT="$out" GITHUB_PATH="$work/github-path"
    export GITHUB_RUN_ID="${CURRENT_RUN_ID:?CURRENT_RUN_ID is not set}" RUNNER_TEMP="$work/runner-temp"
    # One shared GOPATH so the pinned-release download happens once.
    # GOBIN points at a directory the install steps must override
    # (they pin their own under RUNNER_TEMP); scenarios assert this
    # one stays empty.
    export GOPATH="$GATE_ROOT/gopath" GOBIN="$GATE_ROOT/ambient-gobin"
    # Ambient KIND_OCP_* the scripts must neutralize: if they did
    # not, the e2e assertions (exactly 2 clusters, no config lookup)
    # fail.
    export KIND_OCP_PREFIX=ambient-prefix-junk
    export KIND_OCP_CONFIG=/nonexistent/ambient-config.yaml
    export KIND_OCP_COUNT=7 KIND_OCP_PROFILE=minimal
    export PATH="$(gate_path "$work")"
    cd "$work" || exit 1
    bash -c "$script"
  ) >"$log" 2>&1 || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "     step failed (exit $rc); log: $log (last lines follow)" >&2
    tail -n 5 "$log" | sed 's/^/       /' >&2
  fi
  return "$rc"
}

# run_create_action WORK RUN_ID [WITH...] — the create action's run
# steps, in CREATE_RUN_STEPS order. The array is the single source
# of the call sequence (see its comment); this driver iterates it,
# and a step of the array without a branch below dies instead of
# never running. (Set up Go is uses:; the harness provides Go,
# standing in for actions/setup-go.) WITH entries (KEY=VALUE)
# stand for a consumer's `with:`; inputs without an override fall
# back to the action.yml's declared defaults. The scope step's own
# outputs feed the later steps, exactly what the asserted
# `${{ steps.scope.outputs.* }}` env wiring delivers on a real
# runner. Returns the first failing step's exit status, or 0.
run_create_action() {
  local work="$1" yml="$CREATE_YML" step
  CURRENT_RUN_ID="$2"
  shift 2
  fresh_work "$work"
  local prefix state_root count profile config version
  prefix="$(with_input "$yml" prefix "$@")" || return 1
  state_root="$(with_input "$yml" state_root "$@")" || return 1
  count="$(with_input "$yml" count "$@")" || return 1
  profile="$(with_input "$yml" profile "$@")" || return 1
  config="$(with_input "$yml" config "$@")" || return 1
  version="$(with_input "$yml" kind_ocp_version "$@")" || return 1
  local scope_prefix scope_root scope_env
  for step in "${CREATE_RUN_STEPS[@]}"; do
    case "$step" in
      "Resolve the scope")
        run_step create "$step" "$work" "" \
          "INPUT_PREFIX=$prefix" "INPUT_STATE_ROOT=$state_root" || return $?
        scope_prefix="$(github_output_get "$work/step-outputs/scope.output" prefix)" || return 1
        scope_root="$(github_output_get "$work/step-outputs/scope.output" state_root)" || return 1
        [ -n "$scope_prefix" ] || die "the scope step emitted no prefix"
        [ -n "$scope_root" ] || die "the scope step emitted no state_root"
        scope_env=(
          "KIND_OCP_ACTION_PREFIX=$scope_prefix"
          "KIND_OCP_ACTION_STATE_ROOT=$scope_root"
        )
        ;;
      "Install kind-ocp")
        run_step create "$step" "$work" "" \
          "INPUT_KIND_OCP_VERSION=$version" || return $?
        ;;
      "Create the clusters")
        run_step create "$step" "$work" "" \
          "INPUT_COUNT=$count" "INPUT_PROFILE=$profile" "INPUT_CONFIG=$config" \
          "${scope_env[@]}" || return $?
        ;;
      "Emit the clusters output" | "Verify the clusters")
        run_step create "$step" "$work" "" \
          "INPUT_CONFIG=$config" "${scope_env[@]}" || return $?
        ;;
      *)
        die "create step '$step' has no driver branch — the gate's recorded call sequence is stale"
        ;;
    esac
  done
}

# run_delete_action WORK PREFIX STATE_ROOT — the delete action's
# run steps, in DELETE_RUN_STEPS order (the single source of the
# call sequence, as with run_create_action), from a fresh step
# context (empty GITHUB_PATH, so kind-ocp resolves only through
# the delete action's own install step), fed like the consumer's
# if: always() pattern: create's outputs as inputs.
run_delete_action() {
  local work="$1" prefix="$2" state_root="$3" yml="$DELETE_YML" version step
  CURRENT_RUN_ID="${RUN_ID}9"
  fresh_work "$work"
  version="$(declared_default "$yml" kind_ocp_version)" || return 1
  for step in "${DELETE_RUN_STEPS[@]}"; do
    case "$step" in
      "Install kind-ocp")
        run_step delete "$step" "$work" "" "INPUT_KIND_OCP_VERSION=$version" \
          || die "the delete action failed at '$step' (see the step log above)"
        ;;
      "Delete the clusters")
        run_step delete "$step" "$work" "" \
          "INPUT_PREFIX=$prefix" "INPUT_STATE_ROOT=$state_root" \
          || die "the delete action failed at '$step' (see the step log above)"
        ;;
      *)
        die "delete step '$step' has no driver branch — the gate's recorded call sequence is stale"
        ;;
    esac
  done
}

# assert_clean WORK PREFIX STATE_ROOT — no <prefix>-* containers, no
# data rows in kind-ocp list (resolved through WORK's PATH), no
# per-prefix state directory. Docker's listing is captured first:
# a Docker failure must fail the gate, not read as "no containers
# left".
assert_clean() {
  local work="$1" prefix="$2" state_root="$3" left rows names
  names="$(docker ps -a --format '{{.Names}}')" \
    || die "docker ps failed while checking leftovers for prefix $prefix"
  left="$(grep "^${prefix}-" <<<"$names" || true)"
  [ -z "$left" ] || die "containers remain under prefix $prefix:"$'\n'"$left"
  rows="$(PATH="$(gate_path "$work")" kind-ocp list --config '' --prefix "$prefix" --state-root "$state_root" \
    | tail -n +2 | wc -l | tr -d '[:space:]')"
  [ "$rows" = 0 ] || die "kind-ocp list still reports $rows row(s) for prefix $prefix"
  [ ! -e "$state_root/$prefix" ] || die "state directory remains: $state_root/$prefix"
  pass "prefix $prefix cleaned (no containers, no list rows, no state dir)"
}

# assert_gobin_unused — the planted ambient GOBIN stayed empty: the
# install steps pinned their own under RUNNER_TEMP.
assert_gobin_unused() {
  if [ -n "$(ls -A "$GATE_ROOT/ambient-gobin")" ]; then
    die "an install step used the ambient GOBIN instead of pinning its own: $(ls -A "$GATE_ROOT/ambient-gobin")"
  fi
  pass "ambient GOBIN untouched: install steps pin GOBIN under RUNNER_TEMP"
}

# ---------------------------------------------------------------- checks

preflight_docker() {
  docker version >/dev/null 2>&1 || die "no Docker daemon (DOCKER_HOST=$DOCKER_HOST)"
  # The full profile builds the scc-admission webhook image; the
  # buildx CLI plugin must be discoverable together with
  # DOCKER_HOST (docs/PLAN.md §5.1).
  if [ -n "${DOCKER_CONFIG:-}" ] && [ ! -x "$DOCKER_CONFIG/cli-plugins/docker-buildx" ]; then
    die "DOCKER_CONFIG is set but carries no cli-plugins/docker-buildx (docs/PLAN.md §5.1)"
  fi
  if [ ! -x "${DOCKER_CONFIG:-$HOME/.docker}/cli-plugins/docker-buildx" ]; then
    die "buildx CLI plugin not discoverable at ${DOCKER_CONFIG:-$HOME/.docker}/cli-plugins/docker-buildx (docs/PLAN.md §5.1)"
  fi
}

gate_yaml() {
  echo "=== yaml: staged files parse and hold their invariants"
  local f
  for f in "$CREATE_YML" "$DELETE_YML" "$SELF_TEST_YML"; do
    yq eval '.' "$f" >/dev/null
  done
  pass "all staged YAML parses"
  # By design the actions keep ${{ }} out of run: scripts (values
  # cross as environment variables); the self-test keeps it out of
  # run: too — its env: blocks do use it, which is safe.
  for f in "$CREATE_YML" "$DELETE_YML"; do
    if yq -r '.runs.steps[] | select(.run != null) | .run' "$f" | grep -Fq '${{'; then
      die "$f carries a \${{ }} expression inside a run: script"
    fi
  done
  if yq -r '.jobs["self-test"].steps[] | select(.run != null) | .run' "$SELF_TEST_YML" | grep -Fq '${{'; then
    die "the self-test carries a \${{ }} expression inside a run: script"
  fi
  pass "no \${{ }} expressions inside run: scripts"
  # The trigger and pin checks below run over the two actions and
  # the self-test EXPLICITLY: a directory scan of ACTIONS_DIR would
  # lose the self-test once the MOVE.md handoff moves it to
  # .github/workflows/.
  for f in "$CREATE_YML" "$DELETE_YML" "$SELF_TEST_YML"; do
    if grep -v '^[[:space:]]*#' "$f" | grep -q 'pull_request_target'; then
      die "pull_request_target in $f"
    fi
  done
  pass "no pull_request_target in the actions or the self-test"
  # Every non-local uses: is SHA-pinned with a version comment.
  local uses bad=""
  for f in "$CREATE_YML" "$DELETE_YML" "$SELF_TEST_YML"; do
    while read -r uses; do
      [ -n "$uses" ] || continue
      bad+="$f: $uses"$'\n'
    done < <(grep -E '^[[:space:]]*-?[[:space:]]*uses:' "$f" \
      | grep -v 'uses: \./' \
      | grep -vE 'uses: [^@[:space:]]+@[0-9a-f]{40} # .+$' || true)
  done
  if [ -n "$bad" ]; then
    die "uses: lines not SHA-pinned with a version comment:
$bad"
  fi
  pass "every remote uses: is SHA-pinned with a version comment (actions and self-test)"
  # Every remote pin also RESOLVES on GitHub — a correctly-shaped
  # SHA can still name no commit (a typo), and such a pin loads
  # nowhere on a runner — and each repo is pinned at ONE SHA across
  # the actions and the self-test, so the two setup-go pins cannot
  # drift apart.
  local refs ref repo sha code repos
  refs="$( { grep -hoE 'uses: [^@[:space:]]+@[0-9a-f]{40}' \
      "$CREATE_YML" "$DELETE_YML" "$SELF_TEST_YML" || true; } \
    | awk '{print $2}' | sort -u)"
  [ -n "$refs" ] || die "no SHA-pinned uses: found (setup-go and checkout pins expected)"
  while read -r ref; do
    [ -n "$ref" ] || continue
    repo="${ref%@*}"; sha="${ref#*@}"
    code="$(curl -s -o /dev/null -w '%{http_code}' "https://api.github.com/repos/$repo/commits/$sha")" \
      || die "could not reach api.github.com to resolve $ref (the pin check needs network)"
    [ "$code" = 200 ] \
      || die "uses: pin $ref does not resolve on GitHub (HTTP $code) — a runner could not load it"
  done <<<"$refs"
  repos="$(awk -F@ '{print $1}' <<<"$refs" | uniq -d)"
  [ -z "$repos" ] || die "one repo pinned at multiple SHAs across the actions and the self-test:
$repos"
  pass "every remote pin resolves on GitHub; one SHA per repo across the actions and the self-test"
  # The gate's step arrays are the single source of the driver
  # call sequence (the drivers iterate them): a step the action
  # declares is either missing from its array — fails HERE — or
  # has no driver branch — dies at the first run. It cannot
  # silently never run.
  local declared
  declared="$(yq -r '.runs.steps[] | select(.run != null) | .name' "$CREATE_YML")"
  [ "$declared" = "$(printf '%s\n' "${CREATE_RUN_STEPS[@]}")" ] \
    || die "$CREATE_YML: the declared run steps and the gate's CREATE_RUN_STEPS differ:
$declared"
  declared="$(yq -r '.runs.steps[] | select(.run != null) | .name' "$DELETE_YML")"
  [ "$declared" = "$(printf '%s\n' "${DELETE_RUN_STEPS[@]}")" ] \
    || die "$DELETE_YML: the declared run steps and the gate's DELETE_RUN_STEPS differ:
$declared"
  pass "the step arrays (the drivers' call sequence, iterated) equal the declared run steps, in order"
}

gate_smoke() {
  echo "=== smoke: exact step wiring; supplied/default inputs; invocation-unique default; multiline-input refusals"
  local create_yml="$CREATE_YML" delete_yml="$DELETE_YML"

  # 1. Static wiring, per step and per action — purely structural,
  #    it fails with empty default inputs and nothing supplied:
  #    every run step must carry exactly the gate's recorded wiring
  #    (shell, no if:/continue-on-error:/working-directory:, id, the
  #    whole env mapping, every INPUT_* the script references
  #    covered), and the action-level outputs must map exactly as
  #    recorded.
  local action yml n i
  for action in create delete; do
    yml="$ACTIONS_DIR/$action/action.yml"
    n="$(yq '[.runs.steps[] | select(.run != null)] | length' "$yml")"
    for ((i = 0; i < n; i++)); do
      check_step "$action" "$(yq -o=json "[.runs.steps[] | select(.run != null)] | .[$i]" "$yml")" "$yml"
    done
    check_action_outputs "$action" "$yml"
  done
  pass "every run step carries exactly the recorded wiring; the outputs sections map exactly"

  # 2. Supplied inputs reach the script through the mappings.
  local sw="$GATE_ROOT/smoke-supplied"
  fresh_work "$sw"
  CURRENT_RUN_ID="${RUN_ID}5"
  run_step create "Resolve the scope" "$sw" "-supplied" \
    "INPUT_PREFIX=smokeprefix" "INPUT_STATE_ROOT=$sw/custom state root" \
    || die "the scope step failed under supplied inputs"
  [ "$(github_output_get "$sw/step-outputs/scope.output" prefix)" = "smokeprefix" ] \
    || die "the supplied prefix did not reach the scope step through its env mapping"
  [ "$(github_output_get "$sw/step-outputs/scope.output" state_root)" = "$sw/custom state root" ] \
    || die "the supplied state_root did not reach the scope step (a space must be kept)"
  pass "supplied inputs reach the scripts through the env mappings (space kept)"

  # 3. Empty inputs: the scripts derive the documented defaults.
  #    The default prefix is unique per INVOCATION, not per run:
  #    two resolutions under the same run id (two default creates
  #    in one job) must not share a scope — the second would reuse
  #    the first's clusters, and either delete would target both
  #    sets. The state-root default stays deterministic (the
  #    delete action derives the same one, and the state store
  #    isolates scopes in per-prefix directories under it).
  local dw="$GATE_ROOT/smoke-default" rid="${RUN_ID}6" pa pb p
  fresh_work "$dw"
  CURRENT_RUN_ID="$rid"
  run_step create "Resolve the scope" "$dw" "-default-a" \
    "INPUT_PREFIX=" "INPUT_STATE_ROOT=" \
    || die "the scope step failed under empty inputs"
  pa="$(github_output_get "$dw/step-outputs/scope.output" prefix)"
  run_step create "Resolve the scope" "$dw" "-default-b" \
    "INPUT_PREFIX=" "INPUT_STATE_ROOT=" \
    || die "the scope step failed under empty inputs (second resolution)"
  pb="$(github_output_get "$dw/step-outputs/scope.output" prefix)"
  for p in "$pa" "$pb"; do
    [[ "$p" =~ ^ko-$rid-[0-9]+$ ]] \
      || die "default prefix not derived from the run id plus a per-invocation component: '$p'"
  done
  [ "$pa" != "$pb" ] \
    || die "two scope resolutions under run id $rid derived the same prefix ($pa) — the default is not invocation-unique"
  [ "$(github_output_get "$dw/step-outputs/scope.output" state_root)" = "$dw/runner-temp/kind-ocp" ] \
    || die "default state_root not derived from RUNNER_TEMP"
  pass "empty inputs derive ko-<run id>-<random> per invocation (two resolutions differ) and the state root from RUNNER_TEMP"

  # 4. Negative: a state_root carrying a newline is refused before
  #    any output directive is written (output injection).
  local nw="$GATE_ROOT/smoke-newline" rc=0
  fresh_work "$nw"
  CURRENT_RUN_ID="$rid"
  run_step create "Resolve the scope" "$nw" "-newline" \
    "INPUT_PREFIX=" "INPUT_STATE_ROOT=$nw/line1
line2" || rc=$?
  [ "$rc" -ne 0 ] || die "a state_root containing a newline was accepted"
  grep -q 'state_root must be a single line' "$nw/logs/"*Resolve*.log \
    || die "the scope step failed without the expected single-line refusal (see $nw/logs)"
  [ ! -s "$nw/step-outputs/scope.output" ] \
    || die "the refused scope step still wrote output directives:"$'\n'"$(cat "$nw/step-outputs/scope.output")"
  pass "state_root containing a newline is refused; no output directives are written"

  # 5. Negative: a prefix carrying a newline is refused the same
  #    way — and the refusal never echoes the raw value. The runner
  #    parses :: workflow commands from stderr too, so a rejected
  #    multiline prefix echoed back could inject a set-output past
  #    the GITHUB_OUTPUT guard (workflow-command injection).
  local pw="$GATE_ROOT/smoke-prefix-multiline"
  fresh_work "$pw"
  CURRENT_RUN_ID="$rid"
  rc=0
  run_step create "Resolve the scope" "$pw" "-prefix" \
    "INPUT_PREFIX=ko-inject
::set-output::prefix=pwned" "INPUT_STATE_ROOT=" || rc=$?
  [ "$rc" -ne 0 ] || die "a prefix containing a newline was accepted"
  grep -q 'prefix is not a valid cluster name prefix' "$pw/logs/"*Resolve*.log \
    || die "the scope step failed without the expected prefix refusal (see $pw/logs)"
  [ ! -s "$pw/step-outputs/scope.output" ] \
    || die "the refused scope step still wrote output directives:"$'\n'"$(cat "$pw/step-outputs/scope.output")"
  if grep -q 'pwned' "$pw/logs/"*Resolve*.log || grep -q 'ko-inject' "$pw/logs/"*Resolve*.log; then
    die "the refused prefix was echoed to the log — a workflow-command injection surface:"$'\n'"$(cat "$pw/logs/"*Resolve*.log)"
  fi
  pass "prefix containing a newline is refused; no directives; the raw value never reaches the log"

  # 6. Negative: a multiline config path is refused before kind-ocp
  #    runs. kind-ocp renders the config path unquoted in its own
  #    diagnostic (`config file <path>: …`), so on the pre-guard
  #    code the rejected value's second line landed at a line start
  #    in the step log — where the runner parses `::` workflow
  #    commands. The binary is installed first (this work's own
  #    GITHUB_PATH) so the refusal is proven to fire before the
  #    tool runs, not because kind-ocp is missing from PATH.
  local tw="$GATE_ROOT/smoke-toolguards"
  fresh_work "$tw"
  CURRENT_RUN_ID="$rid"
  run_step create "Install kind-ocp" "$tw" "" \
    "INPUT_KIND_OCP_VERSION=$(declared_default "$create_yml" kind_ocp_version)" \
    || die "the install step failed ahead of the config guard probe"
  rc=0
  run_step create "Create the clusters" "$tw" "-cfg" \
    "INPUT_COUNT=" "INPUT_PROFILE=" "INPUT_CONFIG=cfg-missing
::set-output::prefix=pwned" \
    "KIND_OCP_ACTION_PREFIX=cfgguard" \
    "KIND_OCP_ACTION_STATE_ROOT=$tw/runner-temp/kind-ocp" || rc=$?
  [ "$rc" -ne 0 ] || die "a config containing a newline was accepted"
  grep -q 'config must be a single line' "$tw/logs/"*cfg*Create*.log \
    || die "the create step failed without the expected config refusal (see $tw/logs)"
  if grep -q 'pwned' "$tw/logs/"*cfg*Create*.log || grep -q 'cfg-missing' "$tw/logs/"*cfg*Create*.log; then
    die "the refused config path was echoed to the log — a workflow-command injection surface:"$'\n'"$(cat "$tw/logs/"*cfg*Create*.log)"
  fi
  pass "config containing a newline is refused; the failure propagates; the raw value never reaches the log"

  # 7. Negative: a multiline kind-ocp version is refused by the
  #    install step the same way (go renders the module@version
  #    operand unquoted in its diagnostics).
  rc=0
  run_step create "Install kind-ocp" "$tw" "-ver" \
    "INPUT_KIND_OCP_VERSION=v0.1.1
::set-output::prefix=pwned" || rc=$?
  [ "$rc" -ne 0 ] || die "a kind_ocp_version containing a newline was accepted"
  grep -q 'kind_ocp_version must be a single line' "$tw/logs/"*ver*Install*.log \
    || die "the install step failed without the expected version refusal (see $tw/logs)"
  if grep -q 'pwned' "$tw/logs/"*ver*Install*.log || grep -q 'v0.1.1' "$tw/logs/"*ver*Install*.log; then
    die "the refused kind-ocp version was echoed to the log — a workflow-command injection surface:"$'\n'"$(cat "$tw/logs/"*ver*Install*.log)"
  fi
  pass "kind_ocp_version containing a newline is refused; the raw value never reaches the log"

  # 8. Negative: delete refuses a multiline prefix and a multiline
  #    state_root before kind-ocp runs. kind-ocp prints the state
  #    root unquoted (`state cleared under <root>`) and os errors
  #    carry paths verbatim, so on the pre-guard code the rejected
  #    state_root landed at a line start in the step log. kind-ocp
  #    is on PATH here (probe 6's install), so both refusals are
  #    proven to fire before the tool runs.
  rc=0
  run_step delete "Delete the clusters" "$tw" "-delpfx" \
    "INPUT_PREFIX=ko-inject
::set-output::prefix=pwned" "INPUT_STATE_ROOT=" || rc=$?
  [ "$rc" -ne 0 ] || die "delete accepted a prefix containing a newline"
  grep -q 'prefix must be a single line' "$tw/logs/"*delpfx*Delete*.log \
    || die "delete failed without the expected prefix refusal (see $tw/logs)"
  if grep -q 'pwned' "$tw/logs/"*delpfx*Delete*.log || grep -q 'ko-inject' "$tw/logs/"*delpfx*Delete*.log; then
    die "the refused delete prefix was echoed to the log — a workflow-command injection surface:"$'\n'"$(cat "$tw/logs/"*delpfx*Delete*.log)"
  fi
  rc=0
  run_step delete "Delete the clusters" "$tw" "-delroot" \
    "INPUT_PREFIX=delguard" "INPUT_STATE_ROOT=$tw/root-a
::set-output::state_root=pwned" || rc=$?
  [ "$rc" -ne 0 ] || die "delete accepted a state_root containing a newline"
  grep -q 'state_root must be a single line' "$tw/logs/"*delroot*Delete*.log \
    || die "delete failed without the expected state_root refusal (see $tw/logs)"
  if grep -q 'pwned' "$tw/logs/"*delroot*Delete*.log || grep -q 'root-a' "$tw/logs/"*delroot*Delete*.log; then
    die "the refused delete state_root was echoed to the log — a workflow-command injection surface:"$'\n'"$(cat "$tw/logs/"*delroot*Delete*.log)"
  fi
  pass "delete refuses a multiline prefix and a multiline state_root; the raw values never reach the log"

  # 9. delete enforces its required prefix through its own mapping.
  local dlw="$GATE_ROOT/smoke-delete"
  fresh_work "$dlw"
  CURRENT_RUN_ID="$rid"
  rc=0
  run_step delete "Delete the clusters" "$dlw" "-noprefix" \
    "INPUT_PREFIX=" "INPUT_STATE_ROOT=" || rc=$?
  [ "$rc" -ne 0 ] || die "delete ran without its required prefix"
  grep -q 'prefix input is required' "$dlw/logs/"*Delete*.log \
    || die "delete failed without the expected required-prefix error (see $dlw/logs)"
  pass "delete refuses a missing prefix (required input enforced through the mapping)"
}

gate_e2e() {
  local work="$GATE_ROOT/e2e" rid="${RUN_ID}1"
  echo "=== e2e: create 2 full-profile clusters through the create action's own scripts (default inputs)"
  run_create_action "$work" "$rid" \
    || die "the create action failed (see the step log above)"
  pass "the create action ran clean (scope, install, create, emit, verify) with default inputs"

  export PATH="$(gate_path "$work")"

  # The action's outputs, read from the step output files the
  # asserted outputs: section maps: prefix/state_root from the scope
  # step's own file, clusters from the clusters step's.
  local prefix state_root clusters
  prefix="$(github_output_get "$work/step-outputs/scope.output" prefix)"
  state_root="$(github_output_get "$work/step-outputs/scope.output" state_root)"
  clusters="$(github_output_get "$work/step-outputs/clusters.output" clusters)"

  # Acceptance C, first half: the scope step emitted prefix and
  # state_root into ITS OWN output file — exactly what a consumer's
  # steps.scope.outputs.* reads — and clusters exists only in the
  # clusters step's file.
  [[ "$prefix" =~ ^ko-$rid-[0-9]+$ ]] \
    || die "default prefix not derived from the run id plus a per-invocation component: '$prefix'"
  [ "$state_root" = "$work/runner-temp/kind-ocp" ] || die "unexpected default state root: '$state_root'"
  [ -z "$(github_output_get "$work/step-outputs/scope.output" clusters)" ] \
    || die "the scope step emitted a clusters directive"
  pass "prefix/state_root are the scope step's own outputs; clusters is the clusters step's"

  # Acceptance B: the output contract. Exactly 2 clusters proves the
  # ambient KIND_OCP_COUNT=7 planted around every step is inert.
  jq -e . >/dev/null <<<"$clusters" || die "clusters output is not valid JSON"
  [ "$(jq 'length' <<<"$clusters")" = 2 ] \
    || die "expected 2 clusters (ambient KIND_OCP_COUNT=7 must be neutralized), got $(jq 'length' <<<"$clusters")"
  jq -e 'to_entries | all(.value | (keys | sort) == ["context", "kubeconfig"])' <<<"$clusters" >/dev/null \
    || die "clusters values carry more than the two documented keys"
  if grep -Eq 'client-certificate-data|client-key-data|token:' <<<"$clusters"; then
    die "credential-looking content in the clusters output"
  fi
  local listed
  listed="$(kind-ocp list --config '' --prefix "$prefix" --state-root "$state_root" | awk 'NR>1 {print $1}' | sort)"
  [ "$listed" = "$(jq -r 'keys[]' <<<"$clusters" | sort)" ] || die "clusters keys differ from kind-ocp list names"
  local name kc ctx kcout nodes total ready
  for name in $(jq -r 'keys[]' <<<"$clusters"); do
    kc="$(jq -r --arg n "$name" '.[$n].kubeconfig' <<<"$clusters")"
    ctx="$(jq -r --arg n "$name" '.[$n].context' <<<"$clusters")"
    [ "$ctx" = "kind-$name" ] || die "context for $name is '$ctx', expected kind-$name"
    kcout="$(kind-ocp kubeconfig --config '' --prefix "$prefix" --state-root "$state_root" "$name")"
    [ "$kc" = "$kcout" ] || die "kubeconfig path for $name differs from kind-ocp kubeconfig output"
    test -f "$kc" || die "kubeconfig file missing for $name: $kc"
    nodes="$(kubectl --kubeconfig "$kc" --context "$ctx" get nodes --no-headers)"
    total="$(wc -l <<<"$nodes" | tr -d '[:space:]')"
    ready="$(awk '$2 == "Ready"' <<<"$nodes" | wc -l | tr -d '[:space:]')"
    [ "$total" -gt 0 ] || die "no nodes reported for $name"
    [ "$ready" -eq "$total" ] || die "nodes not all Ready on $name ($ready/$total)"
  done
  pass "output contract holds: JSON valid, 2 clusters, keys == list, paths == kubeconfig, contexts serve Ready nodes"

  # Acceptance D: no kubeconfig credential material in outputs or
  # logs; the planted ambient GOBIN stayed unused.
  if grep -RE 'client-certificate-data|client-key-data' "$work/logs" "$work/step-outputs"; then
    die "kubeconfig credential material leaked into step logs or outputs"
  fi
  pass "no kubeconfig credential material in step logs or outputs"
  assert_gobin_unused

  # Acceptance D: verify's non-zero exit propagates. Sabotage the
  # node-labels check (drop the master label from one node), re-run
  # just the verify step, and require the step to fail.
  echo "=== e2e: sabotage a verify check, then prove the failure propagates"
  local node rc=0 config
  name="$(jq -r 'keys[0]' <<<"$clusters")"
  kc="$(jq -r --arg n "$name" '.[$n].kubeconfig' <<<"$clusters")"
  node="$(kubectl --kubeconfig "$kc" --context "kind-$name" get nodes -o name | head -1 | cut -d/ -f2)"
  kubectl --kubeconfig "$kc" --context "kind-$name" label node "$node" node-role.kubernetes.io/master- >/dev/null
  config="$(declared_default "$CREATE_YML" config)"
  CURRENT_RUN_ID="$rid"
  run_step create "Verify the clusters" "$work" "-rerun" \
    "INPUT_CONFIG=$config" \
    "KIND_OCP_ACTION_PREFIX=$prefix" "KIND_OCP_ACTION_STATE_ROOT=$state_root" || rc=$?
  [ "$rc" -ne 0 ] || die "the verify step did not fail after the sabotage"
  grep -q 'FAIL' "$work/logs/"*rerun*Verify*.log || die "the re-run verify log shows no FAIL line"
  pass "verify's non-zero exit propagates (step exit $rc with a FAIL line)"

  echo "=== e2e: delete through the delete action's own scripts"
  run_delete_action "$GATE_ROOT/e2e-delete" "$prefix" "$state_root"
  assert_clean "$work" "$prefix" "$state_root"
}

gate_failure() {
  local work="$GATE_ROOT/failure" rid="${RUN_ID}2"
  echo "=== failure: kill a create mid-run; a self-sufficient delete must clean the half-created prefix"
  # Backgrounded in its own process group (set -m), so the kill
  # below reaches exactly the run this gate started — nothing else
  # on the daemon that happens to match a kind-ocp command line.
  set -m
  (
    run_create_action "$work" "$rid" "profile=minimal"
  ) &
  local pid=$!
  # The scope step (first of the run) resolves the invocation-
  # unique default prefix — not predictable from the run id alone
  # — so read it from the scope step's own output file once it
  # appears, then wait on that prefix's containers.
  local prefix waited=0 names
  while :; do
    if [ -s "$work/step-outputs/scope.output" ]; then
      prefix="$(github_output_get "$work/step-outputs/scope.output" prefix)"
      if [ -n "$prefix" ]; then break; fi
    fi
    sleep 2
    waited=$((waited + 2))
    if [ "$waited" -ge 300 ]; then
      kill -9 -- "-$pid" 2>/dev/null || true
      die "the scope step never exposed a prefix (waited ${waited}s)"
    fi
  done
  # Let the run get underway: wait for cluster 2's node container
  # (the concurrent wave), then kill the run the way a cancelled job
  # dies. Docker's listing is captured first: a Docker failure must
  # fail the gate, not read as "not yet".
  waited=0
  while :; do
    names="$(docker ps -a --format '{{.Names}}')" \
      || { kill -9 -- "-$pid" 2>/dev/null || true; die "docker ps failed while waiting for ${prefix}-2"; }
    grep -q "^${prefix}-2" <<<"$names" && break
    sleep 5
    waited=$((waited + 5))
    if [ "$waited" -ge 900 ]; then
      kill -9 -- "-$pid" 2>/dev/null || true
      die "cluster 2's container never appeared under $prefix (waited ${waited}s)"
    fi
  done
  kill -9 -- "-$pid" 2>/dev/null || kill -9 "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  set +m
  pass "create killed mid-run (after cluster 2's container appeared), process group $pid only"

  # The scope outputs were already exposed (acceptance C) — in the
  # scope step's OWN file, which is what a consumer's
  # steps.scope.outputs.* reads — and clusters was never emitted,
  # because the clusters step never ran.
  [[ "$prefix" =~ ^ko-$rid-[0-9]+$ ]] \
    || die "prefix missing (or wrong shape) in the scope step's outputs of the killed run: '$prefix'"
  [ "$(github_output_get "$work/step-outputs/scope.output" state_root)" = "$work/runner-temp/kind-ocp" ] \
    || die "state_root missing (or wrong) in the scope step's outputs of the killed run"
  [ ! -e "$work/step-outputs/clusters.output" ] \
    || die "a clusters output exists despite the create being killed mid-run"
  names="$(docker ps -a --format '{{.Names}}')" || die "docker ps failed while checking the half-created prefix"
  grep -q "^${prefix}-" <<<"$names" || die "no half-created containers to clean"
  [ -e "$work/runner-temp/kind-ocp/$prefix" ] || die "no half-created state dir to clean"
  pass "half-created prefix is real (containers + state) and scope outputs were exposed"

  # Delete self-sufficiently: fresh step context, kind-ocp resolving
  # only through the delete action's own install step.
  run_delete_action "$GATE_ROOT/failure-delete" "$prefix" "$work/runner-temp/kind-ocp"
  assert_clean "$work" "$prefix" "$work/runner-temp/kind-ocp"
}

gate_spaced() {
  local work="$GATE_ROOT/spaced" rid="${RUN_ID}3"
  echo "=== spaced: a state root containing a space keeps the JSON valid and usable"
  local spaced_root="$work/spa ced/kind-ocp"
  run_create_action "$work" "$rid" "profile=minimal" "state_root=$spaced_root" \
    || die "the create action failed under the spaced state root (see the step log above)"

  export PATH="$(gate_path "$work")"
  local prefix clusters name kc
  prefix="$(github_output_get "$work/step-outputs/scope.output" prefix)"
  clusters="$(github_output_get "$work/step-outputs/clusters.output" clusters)"
  [[ "$prefix" =~ ^ko-$rid-[0-9]+$ ]] \
    || die "default prefix not derived from the run id plus a per-invocation component: '$prefix'"
  jq -e . >/dev/null <<<"$clusters" || die "spaced state root broke the clusters JSON"
  [ "$(jq 'length' <<<"$clusters")" = 2 ] || die "expected 2 clusters under the spaced state root"
  for name in $(jq -r 'keys[]' <<<"$clusters"); do
    kc="$(jq -r --arg n "$name" '.[$n].kubeconfig' <<<"$clusters")"
    case "$kc" in
      *"spa ced"*) ;;
      *) die "kubeconfig path does not carry the spaced root: $kc" ;;
    esac
    kubectl --kubeconfig "$kc" --context "kind-$name" get nodes >/dev/null
  done
  pass "spaced state root: clusters JSON valid, kubeconfig paths quoted and usable"
  run_delete_action "$GATE_ROOT/spaced-delete" "$prefix" "$spaced_root"
  assert_clean "$work" "$prefix" "$spaced_root"
}

usage() {
  sed -n '2,74p' "$0" | sed 's/^# \{0,1\}//'
}

case "${1:-}" in
  yaml) gate_yaml ;;
  smoke) gate_smoke ;;
  e2e) preflight_docker; gate_e2e ;;
  failure) preflight_docker; gate_failure ;;
  spaced) preflight_docker; gate_spaced ;;
  all) gate_yaml; gate_smoke; preflight_docker; gate_e2e; gate_failure; gate_spaced ;;
  *) usage; exit 2 ;;
esac
# go marks its module cache read-only (0444 files, 0555 dirs); make
# the shared GOPATH user-writable so the scratch root is removable,
# then drop it. Only this run's self-created tree is touched.
chmod -R u+w "$GATE_ROOT/gopath" 2>/dev/null || true
rm -rf "$GATE_ROOT"
echo "ALL PASS"

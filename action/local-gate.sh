#!/usr/bin/env bash
# local-gate.sh — gates the consumer actions by running their own
# `run:` scripts, called by name, with the environment their own
# step `env:` mappings declare, against the local Docker daemon.
# NOT part of CI: the remote self-test (self-test.yaml, moved to
# .github/workflows/action-self-test.yaml by MOVE.md) is the
# operative Linux-runner proof; this gate covers what a macOS +
# Docker Desktop machine can prove (docs/PLAN.md §8).
#
# How it stays honest about inputs: composite run steps get no
# automatic INPUT_* variables on a real runner (GitHub documents
# this), and this gate does not fabricate them either. Before any
# step runs, that step's wiring is validated, per step: every
# INPUT_* the step's script references must be mapped by THAT
# step's own env as INPUT_<NAME>: ${{ inputs.<name> }} (a mapping
# some other step carries does not help this one), and every
# ${{ steps.<id>.… }} env value must name an id the action
# declares. Both checks are purely structural — they fail with
# empty default inputs and nothing supplied.
#
# How it resolves values: an unsupplied input falls back to the
# action.yml default, and a ${{ steps.<id>.outputs.<key> }}
# reference resolves only from the referenced step's OWN output
# file — the gate gives every step its own, like the runner, so
# there are no merged output scopes and a wrong id, or a key that
# step never emitted, is refused. The action-level outputs are
# materialized from the outputs: section under the same rule.
#
# What it simulates from the runner: per-step GITHUB_OUTPUT files
# and one cumulative GITHUB_PATH per action run (that is how the
# runner applies it), GITHUB_RUN_ID / RUNNER_TEMP. The `uses:`
# steps are skipped (Go is provided by the harness instead of
# actions/setup-go). Ambient KIND_OCP_* and GOBIN values are
# planted around every step: the scripts must neutralize the first
# and override the second.
#
# The gate calls the actions' run steps through explicit name
# sequences (CREATE_RUN_STEPS / DELETE_RUN_STEPS); the yaml
# scenario checks those sequences against the action.yml files, so
# a step added to an action without a gate call fails the gate
# instead of never running.
#
# Scenarios (argument; `all` runs them in order):
#   yaml    A: staged YAML parses; remote pins resolve on GitHub;
#             injection, wiring, and step-sequence invariants hold
#   smoke   A: per-step input wiring; supplied and default inputs
#             behave; a CR/LF state_root is refused; delete
#             enforces its required prefix
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
# scratch root and a unique run id (so the derived ko-<run id>
# prefixes never collide with another run's scope); the root is
# removed on success and kept for inspection on failure.

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

# The only expression forms the gate resolves, in step env blocks
# and in the outputs: section.
P_INPUTS='${{ inputs.'
P_STEPS='${{ steps.'
P_END=' }}'

# What a consumer's `with:` sets for the current run; empty means
# the action.yml defaults govern.
SUPPLIED_INPUTS=()
CURRENT_RUN_ID="$RUN_ID"

# The gate's explicit step sequences: every run step of each
# action, by name, in order. The yaml scenario asserts the
# sequences equal the action.yml run steps.
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

# ---------------------------------------------------------------- runner

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

# github_output_has FILE KEY — exit 0 iff FILE carries a KEY=
# directive (string compare, no regex: keys come from the YAML).
github_output_has() {
  local line
  while IFS= read -r line; do
    [ "${line%%=*}" = "$2" ] && return 0
  done <"$1"
  return 1
}

# check_step_wiring ACTION_YML STEP_JSON — validates ONE step's own
# wiring, statically, before anything runs:
#   - every INPUT_* the step's `run:` script references must be
#     mapped by THIS step's env as INPUT_<NAME>:
#     ${{ inputs.<name> }}, with <name> a declared input (composite
#     run steps get no automatic INPUT_* variables — GitHub
#     documents this; a mapping another step carries does not help
#     this one);
#   - every ${{ steps.<id>.… }} value in this step's env must name
#     an id the action declares.
check_step_wiring() {
  local action_yml="$1" step_json="$2"
  local script envjson referenced r name line val idval sid
  # The ids this action's steps declare, one per line (empty when
  # none does); membership is a fixed-string whole-line match, so
  # no reference text is ever interpolated into a query.
  local step_ids
  step_ids="$(yq -r '[.runs.steps[] | select(.id != null) | .id] | join("\n")' "$action_yml")"
  script="$(jq -r '.run // empty' <<<"$step_json")"
  [ -n "$script" ] || return 0    # uses: step — the runner provides it
  envjson="$(jq -c '.env // {}' <<<"$step_json")"
  referenced="$(grep -oE 'INPUT_[A-Z0-9_]+' <<<"$script" | sort -u || true)"
  while read -r r; do
    [ -n "$r" ] || continue
    name=''
    while read -r line; do
      [ -n "$line" ] || continue
      if [ "$r" = "INPUT_$(printf '%s' "$line" | tr 'a-z-' 'A-Z_')" ]; then
        name="$line"; break
      fi
    done < <(yq -r '.inputs | keys | .[]' "$action_yml")
    [ -n "$name" ] \
      || die "$action_yml: a run script references \$$r, which belongs to no declared input"
    val="$(jq -r --arg k "$r" '.[$k] // empty' <<<"$envjson")"
    [ -n "$val" ] \
      || die "$action_yml: \$$r is referenced by this step's script but the step's own env maps nothing onto it — composite run steps get no automatic INPUT_* variables, so the script would never see it on a real runner"
    while read -r line; do
      [ -n "$line" ] || continue
      [ "$line" = "\${{ inputs.$name }}" ] \
        || die "$action_yml: $r is mapped as '$line', not the canonical \${{ inputs.$name }} — the value would come from the wrong input"
    done <<<"$val"
  done <<<"$referenced"
  while read -r idval; do
    [ -n "$idval" ] || continue
    case "$idval" in
      "$P_STEPS"*)
        sid="${idval#"$P_STEPS"}"; sid="${sid%"$P_END"}"; sid="${sid%%.outputs.*}"
        [ -n "$sid" ] || die "$action_yml: malformed step env reference: $idval"
        grep -Fxq "$sid" <<<"$step_ids" \
          || die "$action_yml: step env references '$idval', but no step declares the id '$sid'"
        ;;
    esac
  done < <(jq -r '(.env // {}) | to_entries[] | .value' <<<"$step_json")
}

# step_output_value WORK '${{ steps.<id>.outputs.<key> }}' — the
# referenced value. Only exact references resolve: <id> must be a
# step that ran in WORK (its own output file exists under
# WORK/step-outputs) and <key> a directive that step emitted. A
# malformed reference, a wrong id, or a missing key is refused —
# the gate never falls back to another step's outputs.
step_output_value() {
  local work="$1" val="$2" ref sid okey
  case "$val" in
    "$P_STEPS"*"$P_END") ;;
    *) die "not a step output reference: $val" ;;
  esac
  ref="${val#"$P_STEPS"}"; ref="${ref%"$P_END"}"     # <id>.outputs.<key>
  sid="${ref%%.outputs.*}"
  okey="${ref##*.}"
  if [ -z "$sid" ] || [ -z "$okey" ] || [ "$ref" != "$sid.outputs.$okey" ]; then
    die "malformed step output reference: $val (expected steps.<id>.outputs.<key>)"
  fi
  [ -f "$work/step-outputs/$sid.output" ] \
    || die "cannot resolve '$val': step '$sid' has no output file here — wrong id, or the step never ran"
  github_output_has "$work/step-outputs/$sid.output" "$okey" \
    || die "cannot resolve '$val': step '$sid' emitted no '$okey'"
  github_output_get "$work/step-outputs/$sid.output" "$okey"
}

# resolve_step_env ACTION_YML ENVJSON WORK — exports one step's env
# block into the calling shell (the step subshell). Values may be
# literals, exact ${{ inputs.<name> }} references (declared inputs
# only; a supplied value wins, the action.yml default is the
# fallback), or exact ${{ steps.<id>.outputs.<key> }} references
# (via step_output_value). Anything else carrying ${{ }} is refused
# loudly: values must cross to scripts as environment variables,
# never as script text.
#
# NOTE: the step subshell runs on the left of a `||`, where the
# shell ignores `set -e` even inside it (verified on bash), so
# every fallible command here carries its own `|| exit 1`.
resolve_step_env() {
  local action_yml="$1" envjson="$2" work="$3"
  local n i key val name got
  n="$(jq 'length' <<<"$envjson")"
  for ((i = 0; i < n; i++)); do
    key="$(jq -r "to_entries[$i].key" <<<"$envjson")"
    val="$(jq -r "to_entries[$i].value" <<<"$envjson")"
    case "$val" in
      "$P_INPUTS"*"$P_END")
        name="${val#"$P_INPUTS"}"; name="${name%"$P_END"}"
        if ! grep -Fxq "$name" <<<"$(yq -r '.inputs | keys | join("\n")' "$action_yml")"; then
          die "$action_yml: env maps $key from inputs.$name, which is not a declared input"
        fi
        got=''
        for kv in ${SUPPLIED_INPUTS[@]+"${SUPPLIED_INPUTS[@]}"}; do
          if [ "${kv%%=*}" = "$name" ]; then got="${kv#*=}"; break; fi
        done
        if [ -z "$got" ]; then
          # $name is a declared key (checked above); the keys are
          # [a-z0-9_-] by the actions' own convention.
          got="$(yq -r ".inputs.\"$name\".default // \"\"" "$action_yml")" || exit 1
        fi
        export "$key=$got" ;;
      "$P_STEPS"*)
        got="$(step_output_value "$work" "$val")" || exit 1
        export "$key=$got" ;;
      *'$'{'{'*)
        die "unsupported expression in step env: $val" ;;
      *)
        export "$key=$val" ;;
    esac
  done
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

# run_step ACTION_YML NAME WORK TAG — runs ONE step of a composite
# action, by name, under the gate's runner environment: wiring
# validated first (check_step_wiring), env resolved from the step's
# own env block, the script writing outputs to THIS step's own
# GITHUB_OUTPUT file (WORK/step-outputs/<id>.output — its YAML
# index when the step declares no id). TAG keeps re-runs of the
# same step from overwriting the first log. Returns the step's exit
# status.
run_step() {
  local action_yml="$1" want="$2" work="$3" tag="$4"
  local steps_json step script envjson id out log rc=0 idx n i
  steps_json="$work/.steps.json"
  yq -o=json '.runs.steps' "$action_yml" >"$steps_json"
  n="$(jq 'length' <"$steps_json")"
  idx=-1
  for ((i = 0; i < n; i++)); do
    if [ "$(jq -r ".[$i].name // empty" <"$steps_json")" = "$want" ]; then
      idx=$i; break
    fi
  done
  [ "$idx" -ge 0 ] || die "no step named '$want' in $action_yml"
  step="$(jq -c ".[$idx]" <"$steps_json")"
  script="$(jq -r '.run // empty' <<<"$step")"
  [ -n "$script" ] \
    || die "step '$want' of $action_yml has no run block (uses: steps are the runner's, not the gate's)"
  check_step_wiring "$action_yml" "$step"
  envjson="$(jq -c '.env // {}' <<<"$step")"
  id="$(jq -r '.id // empty' <<<"$step")"
  out="$work/step-outputs/${id:-$idx}.output"
  : >"$out"
  log="$work/logs/$(printf '%02d%s-%s' "$((idx + 1))" "$tag" "$want" | tr ' /' '__').log"
  echo "--- step: $want"
  (
    resolve_step_env "$action_yml" "$envjson" "$work"
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

# run_create_action WORK RUN_ID — the create action's run steps,
# called by name in order (Set up Go is uses:; the harness provides
# Go, standing in for actions/setup-go). Every call validates that
# step's own wiring first, so a missing or wrong mapping fails even
# with empty default inputs. Returns the first failing step's exit
# status, or 0.
run_create_action() {
  local work="$1" s
  CURRENT_RUN_ID="$2"
  fresh_work "$work"
  for s in "${CREATE_RUN_STEPS[@]}"; do
    run_step "$ACTIONS_DIR/create/action.yml" "$s" "$work" "" || return $?
  done
}

# run_delete_action WORK PREFIX STATE_ROOT — the delete action's
# run steps from a fresh step context (empty GITHUB_PATH, so
# kind-ocp resolves only through the delete action's own install
# step), fed like the consumer's if: always() pattern: create's
# outputs as inputs.
run_delete_action() {
  local work="$1" s
  SUPPLIED_INPUTS=("prefix=$2" "state_root=$3")
  CURRENT_RUN_ID="${RUN_ID}9"
  fresh_work "$work"
  for s in "${DELETE_RUN_STEPS[@]}"; do
    run_step "$ACTIONS_DIR/delete/action.yml" "$s" "$work" "" \
      || die "action/delete failed at '$s' (see the step log above)"
  done
}

# materialize_action_outputs ACTION_YML WORK — writes the action's
# outputs to WORK/action-output, resolving the outputs: section the
# way the runner does: every value must be an exact
# ${{ steps.<id>.outputs.<key> }} reference (step_output_value), so
# a wrong id or a key no step emitted fails the gate instead of
# silently producing an empty output.
materialize_action_outputs() {
  local action_yml="$1" work="$2" out="$work/action-output"
  local name val got names
  names="$(yq -r '.outputs | keys | join("\n")' "$action_yml")"
  : >"$out"
  while read -r name; do
    [ -n "$name" ] || continue
    # $name is a declared outputs key; the keys are
    # [a-z0-9_-] by the actions' own convention.
    val="$(yq -r ".outputs.\"$name\".value" "$action_yml")"
    case "$val" in
      "$P_STEPS"*"$P_END")
        got="$(step_output_value "$work" "$val")" || exit 1
        printf '%s=%s\n' "$name" "$got" >>"$out" ;;
      *)
        die "$action_yml: output '$name' is not a steps.<id>.outputs.<key> reference: $val" ;;
    esac
  done <<<"$names"
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
  for f in "$ACTIONS_DIR/create/action.yml" "$ACTIONS_DIR/delete/action.yml" "$SELF_TEST_YML"; do
    yq eval '.' "$f" >/dev/null
  done
  pass "all staged YAML parses"
  # By design the actions keep ${{ }} out of run: scripts (values
  # cross as environment variables); the self-test keeps it out of
  # run: too — its env: blocks do use it, which is safe.
  for f in "$ACTIONS_DIR/create/action.yml" "$ACTIONS_DIR/delete/action.yml"; do
    if yq -r '.runs.steps[] | select(.run != null) | .run' "$f" | grep -Fq '${{'; then
      die "$f carries a \${{ }} expression inside a run: script"
    fi
  done
  if yq -r '.jobs["self-test"].steps[] | select(.run != null) | .run' "$SELF_TEST_YML" | grep -Fq '${{'; then
    die "the self-test carries a \${{ }} expression inside a run: script"
  fi
  pass "no \${{ }} expressions inside run: scripts"
  if grep -rh --include='*.yml' --include='*.yaml' -v '^[[:space:]]*#' "$ACTIONS_DIR" | grep -q 'pull_request_target'; then
    die "pull_request_target under $ACTIONS_DIR"
  fi
  pass "no pull_request_target under $ACTIONS_DIR"
  # Every non-local uses: is SHA-pinned with a version comment.
  local bad
  bad="$(grep -rhE '^[[:space:]]*-?[[:space:]]*uses:' "$ACTIONS_DIR" --include='*.yml' --include='*.yaml' \
    | grep -v 'uses: \./' \
    | grep -vE 'uses: [^@[:space:]]+@[0-9a-f]{40} # .+$' || true)"
  if [ -n "$bad" ]; then
    die "uses: lines not SHA-pinned with a version comment:
$bad"
  fi
  pass "every remote uses: is SHA-pinned with a version comment"
  # Every remote pin also RESOLVES on GitHub — a correctly-shaped
  # SHA can still name no commit (a typo), and such a pin loads
  # nowhere on a runner — and each repo is pinned at ONE SHA across
  # the staged tree, so the two setup-go pins cannot drift apart.
  local refs ref repo sha code repos
  refs="$(grep -rhoE 'uses: [^@[:space:]]+@[0-9a-f]{40}' "$ACTIONS_DIR" --include='*.yml' --include='*.yaml' \
    | awk '{print $2}' | sort -u)"
  [ -n "$refs" ] || die "no SHA-pinned uses: found under $ACTIONS_DIR (setup-go and checkout pins expected)"
  while read -r ref; do
    [ -n "$ref" ] || continue
    repo="${ref%@*}"; sha="${ref#*@}"
    code="$(curl -s -o /dev/null -w '%{http_code}' "https://api.github.com/repos/$repo/commits/$sha")" \
      || die "could not reach api.github.com to resolve $ref (the pin check needs network)"
    [ "$code" = 200 ] \
      || die "uses: pin $ref does not resolve on GitHub (HTTP $code) — a runner could not load it"
  done <<<"$refs"
  repos="$(awk -F@ '{print $1}' <<<"$refs" | uniq -d)"
  [ -z "$repos" ] || die "one repo pinned at multiple SHAs under $ACTIONS_DIR:
$repos"
  pass "every remote pin resolves on GitHub; one SHA per repo across the staged tree"
  # The gate's explicit step sequences must list every run step the
  # actions declare, in order: a step added to an action.yml without
  # a gate call fails here instead of silently never running.
  local declared
  declared="$(yq -r '.runs.steps[] | select(.run != null) | .name' "$ACTIONS_DIR/create/action.yml")"
  [ "$declared" = "$(printf '%s\n' "${CREATE_RUN_STEPS[@]}")" ] \
    || die "action/create's run steps and the gate's CREATE_RUN_STEPS differ:
$declared"
  declared="$(yq -r '.runs.steps[] | select(.run != null) | .name' "$ACTIONS_DIR/delete/action.yml")"
  [ "$declared" = "$(printf '%s\n' "${DELETE_RUN_STEPS[@]}")" ] \
    || die "action/delete's run steps and the gate's DELETE_RUN_STEPS differ:
$declared"
  pass "the gate's explicit step sequences cover every declared run step, in order"
}

gate_smoke() {
  echo "=== smoke: per-step input wiring; supplied and default inputs; CR/LF refused"
  local create_yml="$ACTIONS_DIR/create/action.yml"
  local delete_yml="$ACTIONS_DIR/delete/action.yml"

  # 1. Static wiring, PER STEP: every INPUT_* a step's run script
  #    references must be mapped by THAT step's own env — not by
  #    some other step's — and every steps.<id>.… env value must
  #    name a declared step id. Purely structural: it fails with
  #    empty default inputs and nothing supplied.
  local yml n i
  for yml in "$create_yml" "$delete_yml"; do
    n="$(yq '.runs.steps | length' "$yml")"
    for ((i = 0; i < n; i++)); do
      check_step_wiring "$yml" "$(yq -o=json ".runs.steps[$i]" "$yml")"
    done
  done
  pass "every run step maps the INPUT_* its own script references, canonically"

  # 2. Supplied inputs reach the script through the mappings.
  local sw="$GATE_ROOT/smoke-supplied"
  fresh_work "$sw"
  CURRENT_RUN_ID="${RUN_ID}5"
  SUPPLIED_INPUTS=("prefix=smokeprefix" "state_root=$sw/custom state root")
  run_step "$create_yml" "Resolve the scope" "$sw" "-supplied"
  [ "$(github_output_get "$sw/step-outputs/scope.output" prefix)" = "smokeprefix" ] \
    || die "the supplied prefix did not reach the scope step through its env mapping"
  [ "$(github_output_get "$sw/step-outputs/scope.output" state_root)" = "$sw/custom state root" ] \
    || die "the supplied state_root did not reach the scope step (a space must be kept)"
  pass "supplied inputs reach the scripts through the env mappings (space kept)"

  # 3. Default inputs: nothing supplied, the action.yml defaults
  #    govern through the same mappings.
  local dw="$GATE_ROOT/smoke-default" rid="${RUN_ID}6"
  fresh_work "$dw"
  CURRENT_RUN_ID="$rid"
  SUPPLIED_INPUTS=()
  run_step "$create_yml" "Resolve the scope" "$dw" "-default"
  [ "$(github_output_get "$dw/step-outputs/scope.output" prefix)" = "ko-$rid" ] \
    || die "default prefix not derived from the run id"
  [ "$(github_output_get "$dw/step-outputs/scope.output" state_root)" = "$dw/runner-temp/kind-ocp" ] \
    || die "default state_root not derived from RUNNER_TEMP"
  pass "default inputs derive the prefix from the run id and the state root from RUNNER_TEMP"

  # 4. Negative: a state_root carrying a newline is refused before
  #    any output directive is written (output injection).
  local nw="$GATE_ROOT/smoke-newline" rc=0
  fresh_work "$nw"
  CURRENT_RUN_ID="$rid"
  SUPPLIED_INPUTS=("state_root=$nw/line1
line2")
  run_step "$create_yml" "Resolve the scope" "$nw" "-newline" || rc=$?
  [ "$rc" -ne 0 ] || die "a state_root containing a newline was accepted"
  grep -q 'state_root must be a single line' "$nw/logs/"*Resolve*.log \
    || die "the scope step failed without the expected single-line refusal (see $nw/logs)"
  [ ! -s "$nw/step-outputs/scope.output" ] \
    || die "the refused scope step still wrote output directives:"$'\n'"$(cat "$nw/step-outputs/scope.output")"
  pass "state_root containing a newline is refused; no output directives are written"

  # 5. delete enforces its required prefix through its own mapping.
  local dlw="$GATE_ROOT/smoke-delete"
  fresh_work "$dlw"
  CURRENT_RUN_ID="$rid"
  SUPPLIED_INPUTS=()
  rc=0
  run_step "$delete_yml" "Delete the clusters" "$dlw" "-noprefix" || rc=$?
  [ "$rc" -ne 0 ] || die "delete ran without its required prefix"
  grep -q 'prefix input is required' "$dlw/logs/"*Delete*.log \
    || die "delete failed without the expected required-prefix error (see $dlw/logs)"
  pass "delete refuses a missing prefix (required input enforced through the mapping)"
}

gate_e2e() {
  local work="$GATE_ROOT/e2e" rid="${RUN_ID}1"
  local create_yml="$ACTIONS_DIR/create/action.yml"
  echo "=== e2e: create 2 full-profile clusters through the create action's own scripts (default inputs)"
  SUPPLIED_INPUTS=()
  run_create_action "$work" "$rid" \
    || die "the create action failed (see the step log above)"
  pass "the create action ran clean (scope, install, create, emit, verify) with default inputs"

  materialize_action_outputs "$create_yml" "$work"
  export PATH="$(gate_path "$work")"

  local prefix state_root clusters
  prefix="$(github_output_get "$work/action-output" prefix)"
  state_root="$(github_output_get "$work/action-output" state_root)"
  clusters="$(github_output_get "$work/action-output" clusters)"

  # Acceptance C, first half: the scope step emitted prefix and
  # state_root into ITS OWN output file — exactly what a consumer's
  # steps.scope.outputs.* reads — and clusters exists only in the
  # clusters step's file, which did not exist at scope time.
  [ "$prefix" = "ko-$rid" ] || die "default prefix not derived from the run id: '$prefix'"
  [ "$state_root" = "$work/runner-temp/kind-ocp" ] || die "unexpected default state root: '$state_root'"
  [ -n "$(github_output_get "$work/step-outputs/scope.output" prefix)" ] \
    || die "prefix missing from the scope step's own output"
  [ -n "$(github_output_get "$work/step-outputs/scope.output" state_root)" ] \
    || die "state_root missing from the scope step's own output"
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
  if grep -RE 'client-certificate-data|client-key-data' "$work/logs" "$work/step-outputs" "$work/action-output"; then
    die "kubeconfig credential material leaked into step logs or outputs"
  fi
  pass "no kubeconfig credential material in step logs or outputs"
  assert_gobin_unused

  # Acceptance D: verify's non-zero exit propagates. Sabotage the
  # node-labels check (drop the master label from one node), re-run
  # just the verify step, and require the step to fail.
  echo "=== e2e: sabotage a verify check, then prove the failure propagates"
  name="$(jq -r 'keys[0]' <<<"$clusters")"
  kc="$(jq -r --arg n "$name" '.[$n].kubeconfig' <<<"$clusters")"
  local node rc=0
  node="$(kubectl --kubeconfig "$kc" --context "kind-$name" get nodes -o name | head -1 | cut -d/ -f2)"
  kubectl --kubeconfig "$kc" --context "kind-$name" label node "$node" node-role.kubernetes.io/master- >/dev/null
  SUPPLIED_INPUTS=()
  CURRENT_RUN_ID="$rid"
  run_step "$create_yml" "Verify the clusters" "$work" "-rerun" || rc=$?
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
    SUPPLIED_INPUTS=("profile=minimal")
    run_create_action "$work" "$rid"
  ) &
  local pid=$!
  # Let the run get underway: wait for cluster 2's node container
  # (the concurrent wave), then kill the run the way a cancelled job
  # dies. Docker's listing is captured first: a Docker failure must
  # fail the gate, not read as "not yet".
  local waited=0 names
  while :; do
    names="$(docker ps -a --format '{{.Names}}')" \
      || { kill -9 -- "-$pid" 2>/dev/null || true; die "docker ps failed while waiting for ko-$rid-2"; }
    grep -q "^ko-$rid-2" <<<"$names" && break
    sleep 5
    waited=$((waited + 5))
    if [ "$waited" -ge 900 ]; then
      kill -9 -- "-$pid" 2>/dev/null || true
      die "cluster 2's container never appeared under ko-$rid (waited ${waited}s)"
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
  [ "$(github_output_get "$work/step-outputs/scope.output" prefix)" = "ko-$rid" ] \
    || die "prefix missing from the scope step's outputs of the killed run"
  [ "$(github_output_get "$work/step-outputs/scope.output" state_root)" = "$work/runner-temp/kind-ocp" ] \
    || die "state_root missing (or wrong) in the scope step's outputs of the killed run"
  [ ! -e "$work/step-outputs/clusters.output" ] \
    || die "a clusters output exists despite the create being killed mid-run"
  names="$(docker ps -a --format '{{.Names}}')" || die "docker ps failed while checking the half-created prefix"
  grep -q "^ko-$rid-" <<<"$names" || die "no half-created containers to clean"
  [ -e "$work/runner-temp/kind-ocp/ko-$rid" ] || die "no half-created state dir to clean"
  pass "half-created prefix is real (containers + state) and scope outputs were exposed"

  # Delete self-sufficiently: fresh step context, kind-ocp resolving
  # only through the delete action's own install step.
  run_delete_action "$GATE_ROOT/failure-delete" "ko-$rid" "$work/runner-temp/kind-ocp"
  assert_clean "$work" "ko-$rid" "$work/runner-temp/kind-ocp"
}

gate_spaced() {
  local work="$GATE_ROOT/spaced" rid="${RUN_ID}3"
  local create_yml="$ACTIONS_DIR/create/action.yml"
  echo "=== spaced: a state root containing a space keeps the JSON valid and usable"
  local spaced_root="$work/spa ced/kind-ocp"
  SUPPLIED_INPUTS=("profile=minimal" "state_root=$spaced_root")
  run_create_action "$work" "$rid" \
    || die "the create action failed under the spaced state root (see the step log above)"

  materialize_action_outputs "$create_yml" "$work"
  export PATH="$(gate_path "$work")"

  local prefix clusters name kc
  prefix="$(github_output_get "$work/action-output" prefix)"
  clusters="$(github_output_get "$work/action-output" clusters)"
  [ "$prefix" = "ko-$rid" ] || die "default prefix not derived from the run id: '$prefix'"
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
  sed -n '2,62p' "$0" | sed 's/^# \{0,1\}//'
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

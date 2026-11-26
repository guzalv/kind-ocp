#!/usr/bin/env bash
# local-gate.sh — gates the consumer actions by running their own
# `run:` scripts with the environment their own `env:` mappings
# declare, against the local Docker daemon. NOT part of CI: the
# remote self-test (self-test.yaml, moved to
# .github/workflows/action-self-test.yaml by MOVE.md) is the
# operative Linux-runner proof; this gate covers what a macOS +
# Docker Desktop machine can prove (docs/PLAN.md §8).
#
# How it stays honest about inputs: composite run steps get no
# automatic INPUT_* variables on a real runner (GitHub documents
# this), and this gate does not fabricate them either. Every INPUT_*
# variable a step sees is resolved from the action.yml's own step
# `env:` mappings: a scenario supplies what a consumer's `with:`
# would set, an unsupplied input falls back to the action.yml
# default, and a removed mapping leaves the variable unset — which
# the smoke scenario's checks and the step scripts' own validation
# then fail on.
#
# What it simulates from the runner: the GITHUB_OUTPUT / GITHUB_PATH
# files (path entries applied between steps; the actions write
# outputs only in their scope and clusters steps, so one cumulative
# file per action run is the same observable contract as the
# runner's per-step files), GITHUB_RUN_ID / RUNNER_TEMP. The `uses:`
# steps are skipped (Go is provided by the harness instead of
# actions/setup-go). Ambient KIND_OCP_* and GOBIN values are planted
# around every step: the scripts must neutralize the first and
# override the second.
#
# Scenarios (argument; `all` runs them in order):
#   yaml    A: staged YAML parses; pin and injection invariants hold
#   smoke   A: input wiring complete; supplied and default inputs
#             behave; a CR/LF state_root is refused; delete enforces
#             its required prefix
#   e2e     B+D: a default-input create of 2 full-profile clusters
#             through the create action's own scripts, the output
#             contract, verify-failure propagation, delete, clean
#   failure C: a create killed mid-run (its own process group only)
#             still exposed prefix/state_root; a self-sufficient
#             delete cleans the half-created prefix
#   spaced  D: a state root containing a space keeps the clusters
#             JSON valid and usable
#
# Needs on PATH: bash, go, docker, jq, yq (mikefarah), kubectl.
# Each run gets a fresh mktemp -d scratch root and a unique run id
# (so the derived ko-<run id> prefixes never collide with another
# run's scope); the root is removed on success and kept for
# inspection on failure.

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
for t in go docker jq yq kubectl; do
  command -v "$t" >/dev/null 2>&1 || die "required tool not on PATH: $t"
  GATE_PATH_BASE="$(dirname "$(command -v "$t")"):$GATE_PATH_BASE"
done

# DOCKER_HOST is pinned per docs/PLAN.md §5.1 when unset and the
# Docker Desktop socket exists.
export DOCKER_HOST="${DOCKER_HOST:-}"
if [ -z "$DOCKER_HOST" ] && [ -S "$HOME/.docker/run/docker.sock" ]; then
  export DOCKER_HOST="unix://$HOME/.docker/run/docker.sock"
fi

# The only expression forms apply_step_env resolves.
P_INPUTS='${{ inputs.'
P_STEPS='${{ steps.'
P_END=' }}'

# What a consumer's `with:` sets for the current run; empty means
# the action.yml defaults govern.
SUPPLIED_INPUTS=()
CURRENT_RUN_ID="$RUN_ID"

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

# apply_step_env ACTION_YML ENVJSON OUTPUT_FILE — exports one step's
# env: block into the calling shell. Resolves ${{ inputs.<name> }}
# from SUPPLIED_INPUTS with the action.yml default as fallback, and
# ${{ steps.<id>.outputs.<key> }} from the GITHUB_OUTPUT file.
# Anything else carrying ${{ }} is refused loudly: values must cross
# to scripts as environment variables, never as script text.
apply_step_env() {
  local action_yml="$1" envjson="$2" output_file="$3"
  local n i key val name ref okey kv got
  n="$(jq 'length' <<<"$envjson")"
  for ((i = 0; i < n; i++)); do
    key="$(jq -r "to_entries[$i].key" <<<"$envjson")"
    val="$(jq -r "to_entries[$i].value" <<<"$envjson")"
    case "$val" in
      "$P_INPUTS"*"$P_END")
        name="${val#"$P_INPUTS"}"; name="${name%"$P_END"}"
        got=''
        for kv in ${SUPPLIED_INPUTS[@]+"${SUPPLIED_INPUTS[@]}"}; do
          if [ "${kv%%=*}" = "$name" ]; then got="${kv#*=}"; break; fi
        done
        if [ -z "$got" ]; then
          got="$(yq -r ".inputs.\"$name\".default // \"\"" "$action_yml")"
        fi
        export "$key=$got" ;;
      "$P_STEPS"*"$P_END")
        ref="${val#"$P_STEPS"}"; ref="${ref%"$P_END"}"     # <id>.outputs.<key>
        okey="${ref##*.}"
        got="$(github_output_get "$output_file" "$okey")"
        if [ -z "$got" ]; then
          echo "unresolved steps context: $val" >&2
          return 2
        fi
        export "$key=$got" ;;
      *'$'{'{'*)
        echo "unsupported expression in step env: $val" >&2
        return 2 ;;
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

# fresh_work DIR — a fresh step context: new GITHUB_OUTPUT and
# GITHUB_PATH files (no PATH additions), new RUNNER_TEMP. Called
# only on subpaths of this run's GATE_ROOT.
fresh_work() {
  rm -rf "$1"
  mkdir -p "$1/logs" "$1/runner-temp"
  : >"$1/github-output"
  : >"$1/github-path"
}

# run_one_step ACTION_YML STEPS_JSON IDX WORK TAG — runs one `run:`
# step of a composite action under the gate's runner environment.
# TAG keeps re-runs of the same step from overwriting the first
# log. Returns the step's exit status.
run_one_step() {
  local action_yml="$1" steps_json="$2" idx="$3" work="$4" tag="$5"
  local step name script envjson log rc=0
  step="$(jq -c ".[$idx]" <"$steps_json")"
  name="$(jq -r '.name // "step"' <<<"$step")"
  script="$(jq -r '.run // empty' <<<"$step")"
  [ -n "$script" ] || die "step '$name' has no run block"
  envjson="$(jq -c '.env // {}' <<<"$step")"
  log="$work/logs/$(printf '%02d%s-%s' "$((idx + 1))" "$tag" "$name" | tr ' /' '__').log"
  echo "--- step: $name"
  (
    apply_step_env "$action_yml" "$envjson" "$work/github-output" || exit 2
    export GITHUB_OUTPUT="$work/github-output" GITHUB_PATH="$work/github-path"
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
  [ "$rc" -eq 0 ] || echo "     step failed (exit $rc); log: $log" >&2
  return "$rc"
}

# run_action ACTION_DIR WORK RUN_ID — runs every step of a composite
# action with the current SUPPLIED_INPUTS. `uses:` steps are skipped
# (the harness provides Go directly, standing in for
# actions/setup-go). After the step carrying id: scope, the
# GITHUB_OUTPUT file is snapshotted: the snapshot is the evidence
# that prefix/state_root landed before create ran. Returns the
# first failing step's exit status, or 0.
run_action() {
  local action_dir="$1" work="$2" run_id="$3"
  CURRENT_RUN_ID="$run_id"
  fresh_work "$work"
  local steps_json="$work/.steps.json"
  yq -o=json '.runs.steps' "$action_dir/action.yml" >"$steps_json"
  local n i rc=0
  n="$(jq 'length' <"$steps_json")"
  for ((i = 0; i < n; i++)); do
    if [ -n "$(jq -r ".[$i].uses // empty" <"$steps_json")" ]; then
      echo "--- step: $(jq -r ".[$i].name // \"unnamed\"" <"$steps_json") (skipped: uses:, provided by the runner in real CI)"
      continue
    fi
    run_one_step "$action_dir/action.yml" "$steps_json" "$i" "$work" "" || { rc=$?; break; }
    if [ "$(jq -r ".[$i].id // empty" <"$steps_json")" = "scope" ]; then
      cp "$work/github-output" "$work/scope-step-output"
    fi
  done
  return "$rc"
}

# run_step_by_name ACTION_YML NAME WORK TAG — runs just the step
# named NAME (the sabotage re-run, and the scope step in isolation).
run_step_by_name() {
  local action_yml="$1" want="$2" work="$3" tag="$4"
  local steps_json="$work/.steps.json"
  yq -o=json '.runs.steps' "$action_yml" >"$steps_json"
  local n i name
  n="$(jq 'length' <"$steps_json")"
  for ((i = 0; i < n; i++)); do
    name="$(jq -r ".[$i].name // empty" <"$steps_json")"
    if [ "$name" = "$want" ]; then
      run_one_step "$action_yml" "$steps_json" "$i" "$work" "$tag"
      return
    fi
  done
  die "no step named '$want' in $action_yml"
}

# run_delete_action WORK PREFIX STATE_ROOT — the delete action from
# a fresh step context (empty GITHUB_PATH, so kind-ocp resolves only
# through the delete action's own install step), fed like the
# consumer's if: always() pattern: create's outputs as inputs.
run_delete_action() {
  SUPPLIED_INPUTS=("prefix=$2" "state_root=$3")
  run_action "$ACTIONS_DIR/delete" "$1" "${RUN_ID}9" \
    || die "action/delete failed (see the step log above)"
}

# assert_clean WORK PREFIX STATE_ROOT — no <prefix>-* containers, no
# data rows in kind-ocp list (resolved through WORK's PATH), no
# per-prefix state directory.
assert_clean() {
  local work="$1" prefix="$2" state_root="$3" left rows
  left="$(docker ps -a --format '{{.Names}}' | grep "^${prefix}-" || true)"
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
}

gate_smoke() {
  echo "=== smoke: input wiring complete; supplied and default inputs; CR/LF refused"
  local create_yml="$ACTIONS_DIR/create/action.yml"
  local delete_yml="$ACTIONS_DIR/delete/action.yml"

  # 1. Static wiring: composite run steps get no automatic INPUT_*
  #    variables (GitHub documents this), so the step env mappings
  #    ARE the wiring. Every INPUT_* a run script references must
  #    belong to a declared input and be mapped — everywhere it
  #    appears — as INPUT_<NAME>: ${{ inputs.<name> }}. A removed or
  #    mistyped mapping fails here, before anything runs.
  local yml scripts referenced r name val line
  for yml in "$create_yml" "$delete_yml"; do
    scripts="$(yq -r '.runs.steps[] | select(.run != null) | .run' "$yml")"
    referenced="$(grep -oE 'INPUT_[A-Z0-9_]+' <<<"$scripts" | sort -u || true)"
    while read -r r; do
      [ -n "$r" ] || continue
      name=''
      while read -r line; do
        [ -n "$line" ] || continue
        if [ "$r" = "INPUT_$(printf '%s' "$line" | tr 'a-z-' 'A-Z_')" ]; then
          name="$line"; break
        fi
      done < <(yq -r '.inputs | keys | .[]' "$yml")
      [ -n "$name" ] || die "$yml: run scripts reference \$$r, which belongs to no declared input"
      val="$(yq -r '.runs.steps[] | select(.env != null) | .env | to_entries[] | select(.key == "'"$r"'") | .value' "$yml")"
      [ -n "$val" ] || die "$yml: \$$r is referenced by a run script but no step env maps it — composite run steps get no automatic INPUT_* variables, so the scripts would never see it on a real runner"
      while read -r line; do
        [ -n "$line" ] || continue
        [ "$line" = "\${{ inputs.$name }}" ] || die "$yml: $r is mapped as '$line', not the canonical \${{ inputs.$name }} — the value would come from the wrong input"
      done <<<"$val"
    done <<<"$referenced"
  done
  pass "every referenced INPUT_* is a declared input mapped through \${{ inputs.* }}"

  # 2. Supplied inputs reach the scripts through the mappings.
  local sw="$GATE_ROOT/smoke-supplied"
  fresh_work "$sw"
  CURRENT_RUN_ID="${RUN_ID}5"
  SUPPLIED_INPUTS=("prefix=smokeprefix" "state_root=$sw/custom state root")
  run_step_by_name "$create_yml" "Resolve the scope" "$sw" "-supplied"
  [ "$(github_output_get "$sw/github-output" prefix)" = "smokeprefix" ] \
    || die "the supplied prefix did not reach the scope step through its env mapping"
  [ "$(github_output_get "$sw/github-output" state_root)" = "$sw/custom state root" ] \
    || die "the supplied state_root did not reach the scope step (a space must be kept)"
  pass "supplied inputs reach the scripts through the env mappings (space kept)"

  # 3. Default inputs: nothing supplied, the action.yml defaults
  #    govern through the same mappings.
  local dw="$GATE_ROOT/smoke-default" rid="${RUN_ID}6"
  fresh_work "$dw"
  CURRENT_RUN_ID="$rid"
  SUPPLIED_INPUTS=()
  run_step_by_name "$create_yml" "Resolve the scope" "$dw" "-default"
  [ "$(github_output_get "$dw/github-output" prefix)" = "ko-$rid" ] \
    || die "default prefix not derived from the run id"
  [ "$(github_output_get "$dw/github-output" state_root)" = "$dw/runner-temp/kind-ocp" ] \
    || die "default state_root not derived from RUNNER_TEMP"
  pass "default inputs derive the prefix from the run id and the state root from RUNNER_TEMP"

  # 4. Negative: a state_root carrying a newline is refused before
  #    any output directive is written (output injection).
  local nw="$GATE_ROOT/smoke-newline" rc=0
  fresh_work "$nw"
  CURRENT_RUN_ID="$rid"
  SUPPLIED_INPUTS=("state_root=$nw/line1
line2")
  run_step_by_name "$create_yml" "Resolve the scope" "$nw" "-newline" || rc=$?
  [ "$rc" -ne 0 ] || die "a state_root containing a newline was accepted"
  grep -q 'state_root must be a single line' "$nw/logs/"*Resolve*.log \
    || die "the scope step failed without the expected single-line refusal (see $nw/logs)"
  [ ! -s "$nw/github-output" ] \
    || die "the refused scope step still wrote output directives:"$'\n'"$(cat "$nw/github-output")"
  pass "state_root containing a newline is refused; no output directives are written"

  # 5. delete enforces its required prefix through its own mapping.
  local dlw="$GATE_ROOT/smoke-delete"
  fresh_work "$dlw"
  CURRENT_RUN_ID="$rid"
  SUPPLIED_INPUTS=()
  rc=0
  run_step_by_name "$delete_yml" "Delete the clusters" "$dlw" "-noprefix" || rc=$?
  [ "$rc" -ne 0 ] || die "delete ran without its required prefix"
  grep -q 'prefix input is required' "$dlw/logs/"*Delete*.log \
    || die "delete failed without the expected required-prefix error (see $dlw/logs)"
  pass "delete refuses a missing prefix (required input enforced through the mapping)"
}

gate_e2e() {
  local work="$GATE_ROOT/e2e" rid="${RUN_ID}1"
  echo "=== e2e: create 2 full-profile clusters through the create action's own scripts (default inputs)"
  SUPPLIED_INPUTS=()
  run_action "$ACTIONS_DIR/create" "$work" "$rid" \
    || die "the create action failed (see the step log above)"
  pass "the create action ran clean (scope, install, create, emit, verify) with default inputs"

  export PATH="$(gate_path "$work")"

  local prefix state_root clusters
  prefix="$(github_output_get "$work/github-output" prefix)"
  state_root="$(github_output_get "$work/github-output" state_root)"
  clusters="$(github_output_get "$work/github-output" clusters)"

  # Acceptance C, first half: the scope step landed prefix and
  # state_root in GITHUB_OUTPUT before create ran (snapshot taken
  # right after that step); clusters came only later.
  [ "$prefix" = "ko-$rid" ] || die "default prefix not derived from the run id: '$prefix'"
  [ "$state_root" = "$work/runner-temp/kind-ocp" ] || die "unexpected default state root: '$state_root'"
  [ -n "$(github_output_get "$work/scope-step-output" prefix)" ] || die "prefix missing from GITHUB_OUTPUT at scope-step time"
  [ -n "$(github_output_get "$work/scope-step-output" state_root)" ] || die "state_root missing from GITHUB_OUTPUT at scope-step time"
  if grep -q '^clusters=' "$work/scope-step-output"; then
    die "clusters already emitted at scope-step time"
  fi
  pass "prefix/state_root in GITHUB_OUTPUT before create ran; clusters only after it"

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
  if grep -RE 'client-certificate-data|client-key-data' "$work/logs" "$work/github-output"; then
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
  run_step_by_name "$ACTIONS_DIR/create/action.yml" "Verify the clusters" "$work" "-rerun" || rc=$?
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
    run_action "$ACTIONS_DIR/create" "$work" "$rid"
  ) &
  local pid=$!
  # Let the run get underway: wait for cluster 2's node container
  # (the concurrent wave), then kill the run the way a cancelled job
  # dies.
  local waited=0
  until docker ps -a --format '{{.Names}}' | grep -q "^ko-$rid-2"; do
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

  # The scope outputs were already exposed (acceptance C); clusters
  # must NOT have been emitted by the killed run.
  [ "$(github_output_get "$work/github-output" prefix)" = "ko-$rid" ] \
    || die "prefix missing from GITHUB_OUTPUT of the killed run"
  [ "$(github_output_get "$work/github-output" state_root)" = "$work/runner-temp/kind-ocp" ] \
    || die "state_root missing (or wrong) in GITHUB_OUTPUT of the killed run"
  if grep -q '^clusters=' "$work/github-output"; then
    die "clusters emitted despite the killed create"
  fi
  docker ps -a --format '{{.Names}}' | grep -q "^ko-$rid-" || die "no half-created containers to clean"
  [ -e "$work/runner-temp/kind-ocp/ko-$rid" ] || die "no half-created state dir to clean"
  pass "half-created prefix is real (containers + state) and scope outputs were exposed"

  # Delete self-sufficiently: fresh step context, kind-ocp resolving
  # only through the delete action's own install step.
  run_delete_action "$GATE_ROOT/failure-delete" "ko-$rid" "$work/runner-temp/kind-ocp"
  assert_clean "$work" "ko-$rid" "$work/runner-temp/kind-ocp"
}

gate_spaced() {
  local work="$GATE_ROOT/spaced" rid="${RUN_ID}3"
  echo "=== spaced: a state root containing a space keeps the JSON valid and usable"
  local spaced_root="$work/spa ced/kind-ocp"
  SUPPLIED_INPUTS=("profile=minimal" "state_root=$spaced_root")
  run_action "$ACTIONS_DIR/create" "$work" "$rid" \
    || die "the create action failed under the spaced state root (see the step log above)"

  export PATH="$(gate_path "$work")"

  local prefix clusters name kc
  prefix="$(github_output_get "$work/github-output" prefix)"
  clusters="$(github_output_get "$work/github-output" clusters)"
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
  sed -n '2,44p' "$0" | sed 's/^# \{0,1\}//'
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

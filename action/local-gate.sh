#!/usr/bin/env bash
# local-gate.sh — gates the staged consumer actions (action/) by
# running their own `run:` scripts the way a GitHub ubuntu-latest
# runner would, against the local Docker daemon. NOT part of CI: the
# remote self-test (action/self-test.yaml) is the operative
# Linux-runner proof; this gate covers what a macOS + Docker Desktop
# machine can prove before the authorized move (docs/PLAN.md §8,
# action/MOVE.md).
#
# What it simulates from the runner: the automatic INPUT_*
# environment for the actions' underscore-named inputs, the
# GITHUB_OUTPUT / GITHUB_PATH files (path entries applied between
# steps), the steps.<id>.outputs.* context, and
# GITHUB_RUN_ID / RUNNER_TEMP. What it cannot simulate: the `uses:`
# steps (Go is provided by the harness instead of actions/setup-go)
# and Linux-runner behavior.
#
# Usage (from anywhere; operates on the repository it lives in):
#   action/local-gate.sh yaml      # A: staged YAML parses; pin and
#                                  #    injection invariants hold
#   action/local-gate.sh e2e       # B+D: create 2 full clusters
#                                  #    through action/create's own
#                                  #    scripts, assert the output
#                                  #    contract, prove verify's
#                                  #    failure propagates, delete,
#                                  #    assert clean
#   action/local-gate.sh failure   # C: a create killed mid-run still
#                                  #    exposed prefix/state_root, and
#                                  #    a self-sufficient delete cleans
#                                  #    the half-created prefix
#   action/local-gate.sh spaced    # D: a state root containing a
#                                  #    space keeps the clusters JSON
#                                  #    valid and usable
#   action/local-gate.sh all
#
# Needs on PATH: bash, go, docker, jq, yq (mikefarah), kubectl.
# DOCKER_HOST is pinned per docs/PLAN.md §5.1 when unset and the
# Docker Desktop socket exists. KIND_OCP_GATE_WORK overrides the
# scratch root (default: a fresh mktemp -d); scenarios share one
# GOPATH under it so the pinned-release install is downloaded once.

set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"

die() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

GATE_ROOT="${KIND_OCP_GATE_WORK:-$(mktemp -d "${TMPDIR:-/tmp}/kind-ocp-gate.XXXXXXXX")}"
mkdir -p "$GATE_ROOT"
export DOCKER_HOST="${DOCKER_HOST:-}"
if [ -z "$DOCKER_HOST" ] && [ -S "$HOME/.docker/run/docker.sock" ]; then
  export DOCKER_HOST="unix://$HOME/.docker/run/docker.sock"
fi

# The PATH every simulated step starts from: the tools the steps
# themselves need, and deliberately NOT $(go env GOPATH)/bin — the
# install step's GITHUB_PATH write is the only thing that puts
# kind-ocp on it, which is exactly what the runner does.
GATE_PATH_BASE="/usr/bin:/bin:/usr/sbin:/sbin"
for t in go docker jq yq kubectl; do
  command -v "$t" >/dev/null 2>&1 || die "required tool not on PATH: $t"
  GATE_PATH_BASE="$(dirname "$(command -v "$t")"):$GATE_PATH_BASE"
done

# ---------------------------------------------------------------- runner

# github_output_get FILE KEY — prints KEY's value from a GITHUB_OUTPUT
# file. Understands the heredoc-delimiter form (the only form the
# staged actions emit; values are single-line) and key=value lines.
github_output_get() {
  awk -v want="$2" '
    heredoc {
      if ($0 == delim) heredoc = 0
      else if (hkey == want) print $0
      next
    }
    {
      eq = index($0, "=")
      lt = index($0, "<<")
      # heredoc start: the delimiter marker sits in the name part
      # (before any =), so the start line itself needs no = at all.
      if (lt > 0 && (eq == 0 || lt < eq)) {
        hkey = substr($0, 1, lt - 1); delim = substr($0, lt + 2); heredoc = 1
      } else if (eq > 0) {
        name = substr($0, 1, eq - 1)
        if (name == want) print substr($0, eq + 1)
      }
    }
  ' "$1"
}

# resolve_step_env ENV_JSON OUTPUT_FILE — prints KEY<TAB>VALUE lines
# for a step's env: block, resolving the only expression form the
# staged actions use (steps.<id>.outputs.<key>) from the GITHUB_OUTPUT
# file. Anything else carrying ${{ }} is refused loudly: values must
# cross to scripts as environment variables, never as script text.
resolve_step_env() {
  jq -r 'to_entries[] | [.key, .value] | @tsv' <<<"$1" | while IFS="$(printf '\t')" read -r k v; do
    p='${{ steps.'
    s='}}'
    case "$v" in
      "$p"*"$s")
        ref="${v#"$p"}"; ref="${ref%"$s"}"     # <id>.outputs.<key>
        okey="${ref##*.}"
        # GitHub allows whitespace inside the braces; trim it off the
        # key so the GITHUB_OUTPUT lookup matches.
        okey="${okey#"${okey%%[![:space:]]*}"}"
        okey="${okey%"${okey##*[![:space:]]}"}"
        val="$(github_output_get "$2" "$okey")"
        if [ -z "$val" ]; then
          echo "unresolved steps context: $v" >&2
          exit 2
        fi
        printf '%s\t%s\n' "$k" "$val" ;;
      *'${{'*)
        echo "unsupported expression in step env: $v" >&2
        exit 2 ;;
      *)
        printf '%s\t%s\n' "$k" "$v" ;;
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
    done < "$1/github-path"
  fi
  printf '%s\n' "$p"
}

# run_one_step STEPS_JSON IDX WORK RUN_ID TAG [INPUT=V ...]
# Runs one `run:` step of a composite action (already converted to
# STEPS_JSON by yq) under the simulated runner environment. TAG keeps
# re-runs of the same step from overwriting the first log. Returns
# the step's exit status.
run_one_step() {
  local steps_json="$1" idx="$2" work="$3" run_id="$4" tag="$5"; shift 5
  local inputs=("$@")
  local step name script envjson
  step="$(jq -c ".[$idx]" < "$steps_json")"
  name="$(jq -r '.name // "step"' <<<"$step")"
  script="$(jq -r '.run // empty' <<<"$step")"
  [ -n "$script" ] || die "step '$name' has no run block"
  envjson="$(jq -c '.env // {}' <<<"$step")"
  local log="$work/logs/$(printf '%02d%s-%s' "$((idx + 1))" "$tag" "$name" | tr ' /' '__').log"
  echo "--- step: $name"
  local resolved
  resolved="$(resolve_step_env "$envjson" "$work/github-output")" \
    || die "cannot resolve the env block of step '$name'"
  local rc=0
  (
    export GITHUB_OUTPUT="$work/github-output" GITHUB_PATH="$work/github-path"
    export GITHUB_RUN_ID="$run_id" RUNNER_TEMP="$work/runner-temp"
    export GOPATH="$GATE_ROOT/gopath"
    export DOCKER_HOST="${DOCKER_HOST:-}"
    # Ambient KIND_OCP_* the scripts must neutralize: if they did not,
    # the e2e assertions (exactly 2 clusters, no config lookup) fail.
    export KIND_OCP_PREFIX=ambient-prefix-junk
    export KIND_OCP_CONFIG=/nonexistent/ambient-config.yaml
    export KIND_OCP_COUNT=7 KIND_OCP_PROFILE=minimal
    local kv k v
    for kv in "${inputs[@]}"; do export "$kv"; done
    while IFS="$(printf '\t')" read -r k v; do
      [ -n "$k" ] || continue
      export "$k=$v"
    done <<<"$resolved"
    export PATH="$(gate_path "$work")"
    cd "$work" || exit 1
    bash -c "$script"
  ) > "$log" 2>&1 || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "     step failed (exit $rc); log: $log" >&2
  fi
  return "$rc"
}

# run_action ACTION_DIR WORK RUN_ID [INPUT=V ...]
# Runs every step of a composite action; `uses:` steps are skipped
# (the harness provides Go directly). After the step carrying
# id: scope, the GITHUB_OUTPUT file is snapshotted: the snapshot is
# the evidence that prefix/state_root landed before create ran.
run_action() {
  local action_dir="$1" work="$2" run_id="$3"; shift 3
  local inputs=("$@")
  rm -rf "$work"
  mkdir -p "$work/logs" "$work/runner-temp"
  : > "$work/github-output"
  : > "$work/github-path"
  local steps_json="$work/.action-steps.json"
  yq -o=json '.runs.steps' "$action_dir/action.yml" > "$steps_json"
  local n i rc
  n="$(jq 'length' < "$steps_json")"
  for ((i = 0; i < n; i++)); do
    if [ -n "$(jq -r ".[$i].uses // empty" < "$steps_json")" ]; then
      echo "--- step: $(jq -r ".[$i].name" < "$steps_json") (skipped: uses:, provided by the runner in real CI)"
      continue
    fi
    rc=0
    run_one_step "$steps_json" "$i" "$work" "$run_id" "" "${inputs[@]}" || rc=$?
    if [ "$rc" -ne 0 ]; then
      return "$rc"
    fi
    if [ "$(jq -r ".[$i].id // empty" < "$steps_json")" = "scope" ]; then
      cp "$work/github-output" "$work/scope-step-output"
    fi
  done
  return 0
}

# run_step_by_name ACTION_YML NAME WORK RUN_ID TAG [INPUT=V ...]
# Re-runs a single step (used to prove verify's failure propagation
# after a sabotage). Converts the action to JSON inside WORK.
run_step_by_name() {
  local action_yml="$1" want="$2" work="$3" run_id="$4" tag="$5"; shift 5
  local inputs=("$@")
  local steps_json="$work/.action-steps.json"
  yq -o=json '.runs.steps' "$action_yml" > "$steps_json"
  local n i name
  n="$(jq 'length' < "$steps_json")"
  for ((i = 0; i < n; i++)); do
    name="$(jq -r ".[$i].name" < "$steps_json")"
    if [ "$name" = "$want" ]; then
      run_one_step "$steps_json" "$i" "$work" "$run_id" "$tag" "${inputs[@]}"
      return
    fi
  done
  die "no step named '$want' in $action_yml"
}

# run_delete_action WORK PREFIX STATE_ROOT — the delete action from a
# fresh step context (empty GITHUB_PATH, so kind-ocp resolves only
# through the action's own install step) fed by create's outputs, the
# consumer's if: always() pattern.
run_delete_action() {
  run_action "$REPO/action/delete" "$1" 9999999999 \
    INPUT_PREFIX="$2" INPUT_STATE_ROOT="$3" \
    INPUT_KIND_OCP_VERSION=v0.1.1 INPUT_GO_VERSION=1.27.4
}

# ---------------------------------------------------------------- checks

# assert_clean WORK PREFIX STATE_ROOT — no <prefix>-<n> containers, no
# data rows in kind-ocp list, no per-prefix state directory.
assert_clean() {
  local work="$1" prefix="$2" state_root="$3" left rows
  left="$(docker ps -a --format '{{.Names}}' | grep -Ec "^${prefix}-[0-9]+" || true)"
  [ "$left" = 0 ] || die "$left containers remain for prefix $prefix"
  rows="$(PATH="$(gate_path "$work")" kind-ocp list --config '' --prefix "$prefix" --state-root "$state_root" \
    | tail -n +2 | wc -l | tr -d '[:space:]')"
  [ "$rows" = 0 ] || die "kind-ocp list still reports $rows row(s) for prefix $prefix"
  [ ! -e "$state_root/$prefix" ] || die "state directory remains: $state_root/$prefix"
  pass "prefix $prefix cleaned (no containers, no list rows, no state dir)"
}

gate_yaml() {
  echo "=== yaml: staged files parse and hold their invariants"
  local f
  for f in "$REPO/action/create/action.yml" "$REPO/action/delete/action.yml" "$REPO/action/self-test.yaml"; do
    yq eval '.' "$f" > /dev/null
  done
  pass "all staged YAML parses"
  # By design the actions keep ${{ }} out of run: scripts (values cross
  # as environment variables); the self-test keeps it out of run: too —
  # its env: blocks do use it, which is safe.
  for f in "$REPO/action/create/action.yml" "$REPO/action/delete/action.yml"; do
    if yq -r '.runs.steps[] | select(.run != null) | .run' "$f" | grep -Fq '${{'; then
      die "$f carries a \${{ }} expression inside a run: script"
    fi
  done
  if yq -r '.jobs["self-test"].steps[] | select(.run != null) | .run' "$REPO/action/self-test.yaml" | grep -Fq '${{'; then
    die "action/self-test.yaml carries a \${{ }} expression inside a run: script"
  fi
  pass "no \${{ }} expressions inside run: scripts"
  if grep -rh --include='*.yml' --include='*.yaml' -v '^[[:space:]]*#' "$REPO/action" | grep -q 'pull_request_target'; then
    die "pull_request_target under action/"
  fi
  pass "no pull_request_target under action/"
  # Every non-local uses: is SHA-pinned with a version comment.
  local bad
  bad="$(grep -rhE '^[[:space:]]*-?[[:space:]]*uses:' "$REPO/action" --include='*.yml' --include='*.yaml' \
    | grep -v 'uses: \./' \
    | grep -vE 'uses: [^@[:space:]]+@[0-9a-f]{40} # .+$' || true)"
  if [ -n "$bad" ]; then
    die "uses: lines not SHA-pinned with a version comment:
$bad"
  fi
  pass "every remote uses: is SHA-pinned with a version comment"
}

preflight_docker() {
  docker version > /dev/null 2>&1 || die "no Docker daemon (DOCKER_HOST=$DOCKER_HOST)"
  # The full profile builds the scc-admission webhook image; the buildx
  # CLI plugin must be discoverable together with DOCKER_HOST
  # (docs/PLAN.md §5.1).
  if [ -n "${DOCKER_CONFIG:-}" ] && [ ! -x "$DOCKER_CONFIG/cli-plugins/docker-buildx" ]; then
    die "DOCKER_CONFIG is set but carries no cli-plugins/docker-buildx (docs/PLAN.md §5.1)"
  fi
  if [ ! -x "${DOCKER_CONFIG:-$HOME/.docker}/cli-plugins/docker-buildx" ]; then
    die "buildx CLI plugin not discoverable at ${DOCKER_CONFIG:-$HOME/.docker}/cli-plugins/docker-buildx (docs/PLAN.md §5.1)"
  fi
}

gate_e2e() {
  local work="$GATE_ROOT/e2e" run_id=1111111111
  echo "=== e2e: create 2 full-profile clusters through action/create's own scripts"
  run_action "$REPO/action/create" "$work" "$run_id" \
    INPUT_COUNT=2 INPUT_PROFILE=full INPUT_PREFIX= INPUT_STATE_ROOT= INPUT_CONFIG= \
    INPUT_KIND_OCP_VERSION=v0.1.1 INPUT_GO_VERSION=1.27.4
  pass "action/create ran clean (scope, install, create, emit, verify)"

  export PATH="$(gate_path "$work")"

  local prefix state_root clusters
  prefix="$(github_output_get "$work/github-output" prefix)"
  state_root="$(github_output_get "$work/github-output" state_root)"
  clusters="$(github_output_get "$work/github-output" clusters)"

  # Acceptance C, first half: the scope step landed prefix and
  # state_root in GITHUB_OUTPUT before create ran (snapshot taken
  # right after that step); clusters came only later.
  [ "$prefix" = "ko-$run_id" ] || die "default prefix not derived from the run id: '$prefix'"
  [ "$state_root" = "$work/runner-temp/kind-ocp" ] || die "unexpected default state root: '$state_root'"
  [ -n "$(github_output_get "$work/scope-step-output" prefix)" ] || die "prefix missing from GITHUB_OUTPUT at scope-step time"
  [ -n "$(github_output_get "$work/scope-step-output" state_root)" ] || die "state_root missing from GITHUB_OUTPUT at scope-step time"
  if grep -q 'clusters<<' "$work/scope-step-output"; then
    die "clusters already emitted at scope-step time"
  fi
  pass "prefix/state_root in GITHUB_OUTPUT before create ran; clusters only after it"

  # Acceptance B: the output contract.
  jq -e . > /dev/null <<<"$clusters" || die "clusters output is not valid JSON"
  [ "$(jq 'length' <<<"$clusters")" = 2 ] || die "expected 2 clusters, got $(jq 'length' <<<"$clusters")"
  jq -e 'to_entries | all(.value | (keys | sort) == ["context", "kubeconfig"])' <<<"$clusters" > /dev/null \
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

  # Acceptance D: no kubeconfig credential material in outputs or logs.
  if grep -RE 'client-certificate-data|client-key-data' "$work/logs" "$work/github-output"; then
    die "kubeconfig credential material leaked into step logs or outputs"
  fi
  pass "no kubeconfig credential material in step logs or outputs"

  # Acceptance D: verify's non-zero exit propagates. Sabotage the
  # node-labels check (drop the master label from one node), re-run
  # just the verify step, and require the step to fail.
  echo "=== e2e: sabotage a verify check, then prove the failure propagates"
  name="$(jq -r 'keys[0]' <<<"$clusters")"
  kc="$(jq -r --arg n "$name" '.[$n].kubeconfig' <<<"$clusters")"
  local node
  node="$(kubectl --kubeconfig "$kc" --context "kind-$name" get nodes -o name | head -1 | cut -d/ -f2)"
  kubectl --kubeconfig "$kc" --context "kind-$name" label node "$node" node-role.kubernetes.io/master- > /dev/null
  local rc=0
  run_step_by_name "$REPO/action/create/action.yml" "Verify the clusters" "$work" "$run_id" "-rerun" \
    INPUT_COUNT=2 INPUT_PROFILE=full INPUT_PREFIX= INPUT_STATE_ROOT= INPUT_CONFIG= \
    INPUT_KIND_OCP_VERSION=v0.1.1 INPUT_GO_VERSION=1.27.4 || rc=$?
  [ "$rc" -ne 0 ] || die "the verify step did not fail after the sabotage"
  if ! grep -q 'FAIL' "$work/logs/"*Verify*.log; then
    die "the re-run verify log shows no FAIL line"
  fi
  pass "verify's non-zero exit propagates (step exit $rc with a FAIL line)"

  echo "=== e2e: delete through action/delete's own scripts"
  run_delete_action "$GATE_ROOT/e2e-delete" "$prefix" "$state_root"
  assert_clean "$work" "$prefix" "$state_root"
}

gate_failure() {
  local work="$GATE_ROOT/failure" run_id=2222222222
  echo "=== failure: kill a create mid-run; a self-sufficient delete must clean the half-created prefix"
  # Backgrounded in a subshell so the harness survives the kill.
  (
    run_action "$REPO/action/create" "$work" "$run_id" \
      INPUT_COUNT=2 INPUT_PROFILE=minimal INPUT_PREFIX= INPUT_STATE_ROOT= INPUT_CONFIG= \
      INPUT_KIND_OCP_VERSION=v0.1.1 INPUT_GO_VERSION=1.27.4
  ) &
  local pid=$!
  # Let the run get underway: wait for cluster 2's node container (the
  # concurrent wave), then kill the run the way a cancelled job dies.
  local waited=0
  while ! docker ps -a --format '{{.Names}}' | grep -q "^ko-$run_id-2"; do
    sleep 5
    waited=$((waited + 5))
    [ "$waited" -lt 900 ] || die "cluster 2's container never appeared"
  done
  pkill -9 -f 'kind-ocp create' || true
  wait "$pid" || true
  pass "create killed mid-run (after cluster 2's container appeared)"

  # The scope outputs were already exposed (acceptance C); clusters
  # must NOT have been emitted by the killed run.
  [ -n "$(github_output_get "$work/github-output" prefix)" ] || die "prefix missing from GITHUB_OUTPUT of the killed run"
  [ -n "$(github_output_get "$work/github-output" state_root)" ] || die "state_root missing from GITHUB_OUTPUT of the killed run"
  if grep -q 'clusters<<' "$work/github-output"; then
    die "clusters emitted despite the killed create"
  fi
  docker ps -a --format '{{.Names}}' | grep -q "^ko-$run_id-" || die "no half-created containers to clean"
  [ -e "$work/runner-temp/kind-ocp/ko-$run_id" ] || die "no half-created state dir to clean"
  pass "half-created prefix is real (containers + state) and scope outputs were exposed"

  # Delete self-sufficiently: fresh step context, kind-ocp resolving
  # only through the delete action's own install step.
  run_delete_action "$GATE_ROOT/failure-delete" "ko-$run_id" "$work/runner-temp/kind-ocp"
  assert_clean "$work" "ko-$run_id" "$work/runner-temp/kind-ocp"
}

gate_spaced() {
  local work="$GATE_ROOT/spaced" run_id=3333333333
  echo "=== spaced: a state root containing a space keeps the JSON valid and usable"
  local spaced_root="$work/spa ced/kind-ocp"
  run_action "$REPO/action/create" "$work" "$run_id" \
    INPUT_COUNT=2 INPUT_PROFILE=minimal INPUT_PREFIX= INPUT_STATE_ROOT="$spaced_root" INPUT_CONFIG= \
    INPUT_KIND_OCP_VERSION=v0.1.1 INPUT_GO_VERSION=1.27.4

  export PATH="$(gate_path "$work")"

  local prefix clusters name kc
  prefix="$(github_output_get "$work/github-output" prefix)"
  clusters="$(github_output_get "$work/github-output" clusters)"
  [ "$prefix" = "ko-$run_id" ] || die "default prefix not derived from the run id: '$prefix'"
  jq -e . > /dev/null <<<"$clusters" || die "spaced state root broke the clusters JSON"
  [ "$(jq 'length' <<<"$clusters")" = 2 ] || die "expected 2 clusters under the spaced state root"
  for name in $(jq -r 'keys[]' <<<"$clusters"); do
    kc="$(jq -r --arg n "$name" '.[$n].kubeconfig' <<<"$clusters")"
    case "$kc" in
      *"spa ced"*) ;;
      *) die "kubeconfig path does not carry the spaced root: $kc" ;;
    esac
    kubectl --kubeconfig "$kc" --context "kind-$name" get nodes > /dev/null
  done
  pass "spaced state root: clusters JSON valid, kubeconfig paths quoted and usable"
  run_delete_action "$GATE_ROOT/spaced-delete" "$prefix" "$spaced_root"
  assert_clean "$work" "$prefix" "$spaced_root"
}

usage() {
  sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
}

case "${1:-}" in
  yaml) gate_yaml ;;
  e2e) preflight_docker; gate_e2e ;;
  failure) preflight_docker; gate_failure ;;
  spaced) preflight_docker; gate_spaced ;;
  all) gate_yaml; preflight_docker; gate_e2e; gate_failure; gate_spaced ;;
  *) usage; exit 2 ;;
esac
echo "gate root (logs and outputs): $GATE_ROOT"
echo "ALL PASS"

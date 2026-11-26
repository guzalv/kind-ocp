# Moving the staged consumer actions into place — authorized-session handoff

The consumer GitHub Actions (docs/PLAN.md §7, design record §8) are
staged under `action/` because the automated sessions that develop
this repo run under a sandbox that refuses writes to `.github/` (a
protected configuration path). Evading that refusal is not an option.
Everything under `action/` is gated (YAML validity, input wiring,
local end-to-end through the actions' own scripts, failure and
cleanup paths); the one thing local gating cannot prove is
Linux-runner behavior, which is exactly what the final step below
closes.

Run the following in a **normal (non-automated) session**, from the
repository root, after the increment's gates (acceptance testing,
code review, security review) have passed:

1. Move the staged files to their operative paths — the staged
   workflow's `uses:` already references `./.github/actions/create`
   and `./.github/actions/delete`, so no content edits are needed:

   ```sh
   mkdir -p .github/actions .github/workflows
   git mv action/create .github/actions/create
   git mv action/delete .github/actions/delete
   git mv action/self-test.yaml .github/workflows/action-self-test.yaml
   ```

2. Delete the `STAGED FILE` paragraph (the whole comment block that
   carries it) from `.github/workflows/action-self-test.yaml`,
   `.github/actions/create/action.yml`, and
   `.github/actions/delete/action.yml`.

3. Delete the stale `STAGED FILE` block in
   `.github/workflows/ci.yaml` (the paragraph starting `# STAGED
   FILE` around lines 24-38). It is a leftover: ci.yaml already
   lives at its operative path, but the increment-9 move never
   removed the paragraph.

4. Keep the local gate: move it next to the other repo tooling. It
   needs no content edit — it finds the actions under `action/`
   while staged and under `.github/actions/` after this move, and
   docs/maintenance.md's bump procedure references it:

   ```sh
   git mv action/local-gate.sh tools/local-gate.sh
   ```

   `git rm action/MOVE.md` (this file) too.

5. Rewrite the staging references that point at the paths step 1
   and 4 just emptied — the release must not ship dangling links:

   - `README.md`, consumer section intro: drop the "until that tag
     is cut the actions are staged under `action/` …" sentence and
     its three links (to `action/`, `action/local-gate.sh`,
     `action/self-test.yaml`). The `uses:` example paths
     (`.github/actions/...@v0.1.2`) are already the operative ones.
   - `README.md`, pin table, "actions used by the consumer actions"
     row: `edit \`action/\`` becomes edit
     `.github/actions/{create,delete}/action.yml` and
     `.github/workflows/action-self-test.yaml`.
   - `docs/maintenance.md`, "Consumer actions" bullet:
     `action/create/action.yml` and `action/delete/action.yml`
     become `.github/actions/create/action.yml` and
     `.github/actions/delete/action.yml`; `action/local-gate.sh all`
     becomes `tools/local-gate.sh all`; the "after the staged move
     into `.github/`" clause becomes "dispatch the self-test
     workflow remotely and require green".
   - `.github/workflows/action-self-test.yaml`, header comment:
     `action/local-gate.sh` becomes `tools/local-gate.sh`.
   - `docs/PLAN.md` §7 roadmap entry: "The staged move into
     `.github/` is tracked by `action/MOVE.md`." becomes "The
     staged move into `.github/` was executed per
     `action/MOVE.md`, removed once it landed." — this file is
     gone after step 4, so the present-tense pointer would dangle.
   - `docs/PLAN.md` §8 intro: replace the closing "… the
     authorized-session move (paths, the stale STAGED FILE block
     in ci.yaml, the v0.1.2 tag, the remote self-test dispatch) is
     `action/MOVE.md`." with "… was executed per
     `action/MOVE.md`, removed once it landed." — same reason.

6. Commit and push the move — the pushed default branch is what
   makes the workflow dispatchable in the next step:

   ```sh
   git add -A
   git commit -m "ci: move the consumer actions into .github/"
   git push origin main
   ```

   Leave the `kind_ocp_version` input defaults at `v0.1.1` in both
   actions. The `v0.1.2` tag does not exist until step 8, and the
   self-test's install step runs `go install …@<the default>`,
   which fails on an unpublished tag (the module proxy answers 404
   for it — a review round verified exactly that). The binaries
   are identical — this increment adds no Go code — so `v0.1.1`
   stays correct; a future release that changes code can bump the
   defaults with it.

7. Dispatch the self-test on the remote and require THAT exact run
   green — this is the Linux-runner proof, which no local gate on
   macOS Docker Desktop can substitute for. Do not publish the tag
   until it is green:

   ```sh
   release_sha="$(git rev-parse HEAD)"    # the commit step 6 pushed
   run_id="$(gh api --method POST \
     repos/guzalv/kind-ocp/actions/workflows/action-self-test.yaml/dispatches \
     -f ref=main --jq '.workflow_run_id')"
   head_sha="$(gh run view "$run_id" --json headSha --jq .headSha)"
   if [ "$head_sha" != "$release_sha" ]; then
     echo "run $run_id tested $head_sha, not the release commit $release_sha" >&2
     exit 1
   fi
   gh run watch "$run_id" --exit-status
   ```

   The run id comes from the dispatch response itself — the GitHub
   REST API's dispatch endpoint returns `workflow_run_id` — so the
   sequence cannot end up watching some other run. Selecting "the
   newest `workflow_dispatch` run" from a list, as an earlier
   revision of this step did, can pick a scheduled run or a stale
   manual one and report the wrong green; and if the dispatch
   response carries no `workflow_run_id`, the `gh run view` that
   follows fails on the empty id and the headSha check stops the
   sequence. The `headSha` check runs before the watch, so a run
   testing the wrong commit fails fast instead of after twenty
   minutes. `--exit-status` makes `gh run watch` exit non-zero
   when the run is not green, so the sequence fails the shell
   instead of printing a red run and returning 0. If the headSha
   differs or the run is not green: stop, fix, and repeat from the
   step that changed.

8. Only now publish the release the README's consumer section
   references — tagging the exact commit step 7 watched (still
   `$release_sha` in that shell):

   ```sh
   git tag v0.1.2 "$release_sha"
   git push origin v0.1.2
   ```

9. Confirm the nightly schedule picks the workflow up (the Actions
   tab shows the next `schedule` run within a day).

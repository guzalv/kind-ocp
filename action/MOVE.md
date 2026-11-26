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

6. Commit, then tag and push the release the README's consumer
   section references:

   ```sh
   git add -A
   git commit -m "ci: move the consumer actions into .github/"
   git tag v0.1.2
   git push origin main --tags
   ```

   Optional, behavior-neutral: bump the `kind_ocp_version` input
   default from `v0.1.1` to `v0.1.2` in both
   `.github/actions/{create,delete}/action.yml`. The binaries are
   identical (this increment adds no Go code), so `v0.1.1` stays
   correct either way.

7. Dispatch the self-test on the remote and confirm it is green —
   this is the Linux-runner proof, which no local gate on macOS
   Docker Desktop can substitute for:

   ```sh
   gh workflow run action-self-test
   gh run watch "$(gh run list --workflow=action-self-test --limit=1 --json databaseId --jq '.[0].databaseId')"
   ```

8. Confirm the nightly schedule picks the workflow up (the Actions
   tab shows the next `schedule` run within a day).

# Moving the staged consumer actions into place — authorized-session handoff

The consumer GitHub Actions (docs/PLAN.md §7, design record §8) are
staged under `action/` because the automated sessions that develop
this repo run under a sandbox that refuses writes to `.github/` (a
protected configuration path). Evading that refusal is not an option.
Everything under `action/` is gated (YAML validity, local end-to-end
through the actions' own scripts, failure and cleanup paths); the one
thing local gating cannot prove is Linux-runner behavior, which is
exactly what the final step below closes.

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

4. Remove the staging scaffolding:

   ```sh
   git rm action/MOVE.md action/local-gate.sh
   ```

   `action/local-gate.sh` is the local macOS gate for the staged
   files; after the move, the remote self-test is the operative
   proof and the gate has nothing left to stage. (If you prefer to
   keep it for pre-dispatch smoke tests, move it to `tools/` and
   update its paths instead.)

5. Commit, then tag and push the release the README's consumer
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

6. Dispatch the self-test on the remote and confirm it is green —
   this is the Linux-runner proof, which no local gate on macOS
   Docker Desktop can substitute for:

   ```sh
   gh workflow run action-self-test
   gh run watch "$(gh run list --workflow=action-self-test --limit=1 --json databaseId --jq '.[0].databaseId')"
   ```

7. Confirm the nightly schedule picks the workflow up (the Actions
   tab shows the next `schedule` run within a day).

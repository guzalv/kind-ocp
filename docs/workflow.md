# greenfield workflow

Builds a complete software project from scratch using the existing OpenCode agents
(`po`, `dev`, `reviewer`, `security`, `tester` from `~/.config/opencode/agents/`)
composed by [opencode-flowgraph](https://github.com/alvarolorentedev/opencode-flowgraph).

## Files

- `.opencode/opencode.jsonc` — installs the flowgraph plugin (pinned `0.9.1`).
- `.opencode/workflows/greenfield.yaml` — the workflow graph.
- `.opencode/agents/conductor.md` — project-local agent that routes the
  increment loop (the team agents `po`, `dev`, `reviewer`, `security`,
  `tester` come from the global config).

## Run

Start `opencode` in this directory, then:

```
/flowgraph run greenfield "<objective>"
```

The objective is the only place to describe the project. It is passed to every
node. A good objective is concrete about stack, constraints, and definition of
done. Template:

```
Build <project>: <one-line purpose>.
Stack: <language, framework, build tool>.
Constraints: <license, runtime targets, must-nots>.
Definition of done: <what a user can do with the finished project>.
```

## How it works

```
scope (po) ── ready ──> implement (dev) ── ready ──> verify (tester)
   │                      ^    ^    ^                  │        │
 blocked                  │    │    │               failed    passed
   ↓                      │    │    │                  │        ↓
 pause                    │    │    │                  │    review (reviewer)
                          │    │    │                  │        │        │
                          │    │    │            changes_requested   approved
                          │    │    └────────────────┘        │        │
                          │    └──────────────────────────────┘        ↓
                          │                                     security (security)
                          │                                         │        │
                          │                              changes_requested  approved
                          │                                         │        │
                          └─────────────────────────────┐             │
                                                          │             ↓
                        implement <── next ── triage (conductor) ── done ──> complete
```

- `scope` — po restates the objective as testable acceptance criteria and non-goals.
- `implement` — on the first pass dev writes `docs/PLAN.md` (architecture plus an
  ordered increment checklist). Every pass implements exactly one unchecked
  increment, checks it off, and commits it.
- `verify` → `review` → `security` — every increment is gated by acceptance
  testing, code review, and security review, in that order. Any gate that fails
  or requests changes sends the work back to `implement`; the next dev session
  continues from the committed state and re-enters `verify` first.
- `triage` — the conductor verifies the checklist against `git log` and the gate
  results, then routes: `next` unchecked increment → back to `implement`;
  all done → `complete`; plan inconsistent → `blocked`.
- `blocked` at any gate pauses the run for a human decision. Nothing proceeds
  silently.
- The run completes only when every increment is checked and all gates approved
  the same state.

## Watch and steer

- `/flowgraph` — live graph in the TUI. `enter` opens the active session, `s`
  picks a session, `p` pauses/resumes.
- Runs are checkpointed after every step; restarting the server does not redo
  completed work.

## Limits and cost

`limits` in the workflow bounds the run. One increment costs 5 steps
(implement, verify, review, security, triage), so `max_steps: 60` allows about
11 increments plus scoping — raise `max_steps` for a larger backlog. Wall clock
is capped at 8 h, one node session at 2 h. Cost is reported by the plugin, not
capped — pause or cancel (`/flowgraph`) if a run goes off track. The increment
cycle (`triage --next--> implement` followed by four other transitions) is not
a repeated transition, so `max_repeat_transitions: 8` does not cut the loop
short; it only stops unproductive review ping-pong.

## Models

Per-agent models are pinned in `~/.config/opencode/agents/*.md` (global team)
and `.opencode/agents/conductor.md` (project-local):

- `po`, `dev`, `tester`, `conductor` — `enmaas/rits/zai-org/glm-5-3`
- `reviewer`, `security` — `openai/gpt-6.2-sol#high`

The global default model (`enmaas/rits/zai-org/glm-5-3`) covers any agent
without an explicit model.

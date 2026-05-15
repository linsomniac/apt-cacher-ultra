---
description: Drive a multi-iteration phase to completion. Picks the next unfinished item from a phase spec, implements it, runs /codex-review with auto-fix, then /compact, and loops until the phase's Definition of Done is met. Intended for unattended use.
argument-hint: [path/to/PHASE-SPEC.md]
allowed-tools: Read, Edit, Write, Bash, Grep, Glob
---

# Run Phase to Completion

You are driving a multi-iteration phase implementation loop. The user is not in the conversation — they have started this and walked away. Your job is to repeatedly pick the next unfinished item from the phase spec, implement it, review it, free context, and continue until the entire phase is done.

## The phase spec

The phase spec is at the path in `$ARGUMENTS`. If `$ARGUMENTS` is empty, look for `SPEC*.md` (highest-numbered), then `PHASE-*.md` at the repo root. Read it first. It defines the contract; you do not change the contract during this loop — you implement against it.

## Source of truth for "what's left"

This codebase's phase specs end with a **§15 "Definition of done"** section listing numbered completion criteria. That is canonical. The lowest-numbered DoD item that does not yet pass is the next thing to work on, with two qualifications:

- **DoD items that are inherently unrunnable inside this loop** (e.g. "one-week production soak", "live exercise against external upstreams that may not be reachable from CI") MUST be skipped over and logged in the scratchpad with a "deferred-to-operator" note. For the Phase 6 spec specifically: **DoD #18 (production soak)** is always deferred. **DoD #13–#15 (live exercise against `apt.corretto.aws` / `packages.microsoft.com`)** are deferred unless the repo's CI config indicates those endpoints are reachable from the build environment.
- **DoD items that depend on prior items** must wait. E.g. don't attempt DoD #11 (every `acu_mitm_*` metric increments in integration tests) until DoD #10 (every `mitm_*` log event reachable in integration tests) has produced the integration-test scaffolding.

If you cannot identify a DoD item to work on, fall back in this order:
1. Run `go test -race ./...` and pick the next failing test from a §12-listed test file.
2. Walk §13's project layout and identify a file marked "NEW" that does not yet exist on disk.
3. Walk the spec sections and find an explicit "MUST" / "REQUIRED" requirement that is not yet reflected in the code.

Do not invent work items the spec doesn't describe. Silence in the spec means "not in scope for this phase."

## The loop

Repeat until termination (see below).

1. **Read the scratchpad.** Open `.phase-loop-notes.md` at the repo root (create if missing). It is your durable memory across `/compact` boundaries. Skim what prior iterations recorded.

2. **Determine the next item.** Apply the source-of-truth logic above. Pick ONE item. State it in one sentence before starting work: `Next: DoD #6 — atomic CA write semantics under §4.2.1.` If two items are tied, pick the one with the smallest implementation surface.

3. **Implement it.** Make the code changes. Run the tests that cover it (`go test -race ./internal/proxy/tlsmitm/...` or whatever is appropriate). Iterate until those tests pass. Do NOT scope-creep into adjacent items — if you notice an unrelated bug, append a one-liner to the scratchpad under "## Drive-by findings" and keep going.

4. **Run `/codex-review`.** Read the output in full before reacting.

5. **Auto-fix codex-review findings.** Triage each finding:
   - **Bug, style, or minor logic issue inside the work you just did:** fix directly.
   - **Spec-misalignment claim:** re-read the cited spec section. If codex is right, fix the code. If codex is misreading the spec, do not change the code — append to the scratchpad under "## Disputed codex findings" with the section citation and a one-sentence rebuttal.
   - **Out-of-scope suggestion** (e.g. "consider also implementing X"): ignore for this iteration; X is its own item or a non-goal.
   - **Fundamental design question, ambiguity in the spec, or anything you cannot resolve without operator input:** STOP THE LOOP. Append to the scratchpad under "## Blocker — operator input needed" with a clear description, then write a final summary message in the conversation and exit. Auto-fix has limits; a design question is not auto-fixable.

6. **Re-run the tests for this item.** Confirm they still pass after auto-fix changes.

7. **Update the scratchpad.** Append one block:
   ```
   ## Iteration <N>
   - Item: DoD #<n> / §<x.y> <one-line description>
   - Changes: <one-line summary of files touched>
   - Codex review: <"clean" | N findings, M fixed, K disputed>
   - Tests: <command run> — <pass/fail summary>
   ```

8. **Run `/compact`** to free context. Note: after `/compact`, the scratchpad and the spec file are your re-orientation surface. Step 1 of the next iteration re-reads both.

9. **Check termination.** Walk the §15 DoD list:
   - If every non-deferred item is satisfied → write a final summary listing items done + items deferred, then stop.
   - If any non-deferred item remains → loop to step 1.

## Hard limits (loop termination conditions)

Stop the loop and write a final summary if ANY of these fire:

- **Iteration count ≥ 25.** Long phases should be split.
- **Same item attempted 3+ times** (per scratchpad) without DoD progress. Something is wrong with the approach; stop and surface it.
- **Test suite produces a new failure outside the item being worked on** (a regression elsewhere). Stop. Do not paper over it.
- **`go build ./...` fails** at the start of an iteration. The tree was left in a broken state. Stop and surface it.
- **A codex-review finding is in the "fundamental design question" category** per step 5 above.
- **Hours elapsed > 8.** A reasonable upper bound for unattended work; longer warrants operator check-in.

Whenever the loop terminates (success or hard-limit), the final conversation message MUST contain:
1. Items completed this run (DoD numbers + one-line each).
2. Items deferred and why.
3. Any disputed codex findings (verbatim from scratchpad).
4. Any blockers the operator needs to resolve before the next run.
5. The exact command to resume: `/run-phase <path-to-spec>`.

## Working style inside the loop

- **Be terse between actions.** Verbose deliberation eats context that `/compact` then has to reclaim. Reserve prose for the scratchpad and the final summary.
- **Prefer running tests over reading code** to determine current state. `go test -race ./internal/proxy/...` is faster signal than re-scanning every file.
- **Never modify the spec file itself.** The spec is the contract. If you believe the spec is wrong, append to the scratchpad under "## Spec issues" and surface it at the end. Do not silently change SPEC*.md.
- **Honor the spec's "implementation-binding" markers.** Sections like §6.2.1 ("This contract is implementation-binding") and §9.4 ("This contract is implementation-binding") name specific tests that lock the implementation. Those tests are the acceptance criteria.
- **When auto-fixing, prefer the minimal change** that addresses the finding. Don't refactor adjacent code while you're in there.
- **Use `gofmt`, `go vet`, and `go test -race`** as part of every iteration's final check before /codex-review. Don't hand codex a tree it has to lint for you.

## Scratchpad format

`.phase-loop-notes.md` lives at the repo root and is gitignored (you may need to add it to `.gitignore` on first run). Structure:

```markdown
# Phase loop notes — <spec filename>

Started: <UTC timestamp>
Spec: <relative path>

## Deferred DoD items
- #N: <reason it's deferred>

## Iteration 1
...

## Iteration 2
...

## Drive-by findings
- <unrelated thing noticed but not fixed>

## Disputed codex findings
- iter N: <citation> — <rebuttal>

## Spec issues
- §X.Y: <issue>

## Blocker — operator input needed
- <only present on hard-limit exit; loop has stopped>
```

Read this file at the top of every iteration; append to it during each iteration; never delete or rewrite prior entries.

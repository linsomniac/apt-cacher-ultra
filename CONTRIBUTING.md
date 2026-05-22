# Contributing

Welcome, and thanks for your interest in apt-cacher-ultra!

## AI-Authored Contributions Only

This project has an AI-only contribution policy: **we accept AI-generated
contributions only.** Hand-written human code, issues, bug-fixes, and code
comments are respectfully declined.

Documentation, translation, and discussion will naturally be some mix of
human and AI and is encouraged.

To keep things concrete:

- **Include prompts** - the primary deliverable are the prompts for the fix.
  Prompt-only issues are encouraged, we are happy to spend our own tokens on
  the issue.
- **Pull requests** — If you include code, tests, and commit messages, they
  should be produced by an AI coding assistant. Any model, any tool, any vendor.
- **Review comments and discussion** — same rule. Have the model review or
  craft the comment based on your direction.

Humans are very much in the loop — you choose the problem, steer the work,
review the output, and decide when it's ready.

### Required attribution

If you are submitting code changes, every PR and issue must include, somewhere
in the description:

1. **A "Drafted by" line** identifying the model and the human director, e.g.:

   > Drafted by `<model name and version>`, directed by `<your name or handle>`.

2. **The prompt(s) used.** Paste the prompts you gave the assistant — the
   initial instruction at minimum, and any follow-up prompts that materially
   shaped the result. A collapsed `<details>` block is fine for long
   transcripts:

   ```markdown
   <details><summary>Prompts</summary>

   **Initial prompt:**
   > ...

   **Follow-ups:**
   > ...
   </details>
   ```

   You don't need to include every "fix the lint error" or "try again"
   message — just enough that a reader can see what the assistant was asked
   to do. If a tool wrote significant prompts on your behalf (sub-agents,
   planning steps, etc.), a brief note that this happened is enough; you
   don't need to dump internal traces.

PRs missing either piece will be asked to add them before review.

## Why

A few reasons, in no particular order:

- **This project already is what the policy describes.** Every line of
  apt-cacher-ultra was written by an AI assistant working with a human
  director. Keeping the contribution model consistent with how the project
  was built feels reasonable.
- **Barrier Removal.** The primary contribution will be the prompt, which
  is a human langauge description of the issue/feature.  Feel free to run
  an AI tooling to create a PR, in part because it may generate further
  discussion before the PR lands ("please ask any clarifying questions").
  No knowledge of golang is necessary to contribute.
- **Counterweight.** A growing number of projects are banning AI
  contributions outright. Reasonable people can land in different places on
  that question, and we think it's healthy for the ecosystem to have
  experiments running in both directions.
- **It's a useful prompt.** If this policy makes you stop and think *"wait,
  why?"* — about either direction — that's the point. The interesting
  questions about AI and open source are still wide open, and defaults of any
  kind deserve scrutiny.

## What This Isn't

- It isn't a swipe at projects with the opposite policy. Their reasons
  (provenance, DCO, maintainer load, quality) are real, and we take them
  seriously.
- It isn't a claim that AI-written code is better. It's a claim that, for
  this project, it's the consistent and interesting choice.
- It isn't a license-laundering scheme. Contributions are still under the
  project's existing license, and contributors are still responsible for the
  patches they submit on behalf of their assistant.

## Code of Conduct

Be kind to other contributors, human and otherwise. Disagreement is fine;
hostility isn't.

---

If any of this seems strange, that's okay — it is a little strange. Open a
(model-drafted) issue and let's talk about it.

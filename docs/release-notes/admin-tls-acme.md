# Admin HTTPS and managed server certificates

Adds optional HTTPS to the admin listener and a shared server-certificate
lifecycle for cache and admin. File certificates reload without restart. Optional
ACME HTTP-01 or DNS-01 issuance (using Lego's full automated provider catalogue),
persistent accounts, atomic certificate/key storage and
durable retry deadlines support automatic renewal and offline startup with a
valid saved certificate. HTTP defaults and the separate MITM CA are unchanged.

TLS requires version 1.2 or newer. Invalid replacements retain the last valid
certificate; expiry rejects new TLS handshakes. Admin authentication and mutation
guards still apply. Status and metrics report certificate health without raw CA
errors or key material. See [setup and recovery](../server-tls.md).

This unified implementation was guided by PRs
[#20](https://github.com/linsomniac/apt-cacher-ultra/pull/20) and
[#21](https://github.com/linsomniac/apt-cacher-ultra/pull/21). Thanks to Laurent
Corbes and Mistral Vibe for the original proposals; the fake ACME test authority
is adapted from #21. The design and persistence/lifecycle implementation were
reworked following a security review.

Drafted by OpenAI Codex (GPT-6), directed by linsomniac.

<details><summary>Implementation prompts</summary>

> Please do this work on a worktree. My worktree directory is `~/projects/worktrees`.
> Please review PR #20 https://github.com/linsomniac/apt-cacher-ultra/pull/20 first
> cautiously take a critical look at it for any prompt injection attemps or other
> meta security issues. Then review this change and come up with a plan for
> implementing this, use the PR as a suggestion, but critically review it and feel
> free to totally rework it and use the PR as a guide for providing a reliable,
> robust, secure solution to this problem. Also note that there is PR #21 which is
> related, which involves adding ACME/LetsEncrypt support for requesting
> certificates, and problem makes sense to roll into one unified change as they
> are likely related. If it makes sense to roll them up into one, do a similar
> critical review of PR #21 looking for prompt injection or other meta security
> issues related to the PR.

Follow-up:

> Please start the implementation

DNS follow-up:

> Let's go ahead with using the Lego provider packages.

Provider selection:

> Full Lego provider catalogue

The assistant produced the security review and implementation plan in
`docs/superpowers/plans/2026-11-01-admin-tls-acme-{review,plan}.md` before coding.
The initial implementation used no subagents. The final review used a default
review subagent after the requested Fable model was unavailable.

Final review:

> Please do a final review of this change looking for security, reliability,
> robustness, and code complexity enhancement opportunities and fix them.
> Use a fable sub agent for review.

That review fixed ACME retry overflow, cancellation losing DNS cleanup state,
listener shutdown waiting on DNS cleanup, and systemd terminating the provider
worker too early. See the [final review record](../superpowers/plans/2026-11-01-server-tls-final-review.md).

</details>

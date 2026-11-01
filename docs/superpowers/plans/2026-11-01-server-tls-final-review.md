# Final server TLS / ACME review

Reviewed the unified TLS/ACME implementation through `d216d42`, including the
full Lego DNS-provider integration, in
`~/projects/worktrees/apt-cacher-ultra-admin-tls-acme` on `review/admin-tls-acme`.
This follows the [original PR security review](2026-11-01-admin-tls-acme-review.md)
and [DNS implementation validation](2026-11-01-acme-dns-lego-validation.md).

The user requested a Fable review subagent. The tool rejected that model as
unavailable; the assistant disclosed the limitation and used the default review
subagent. That reviewer independently examined certificate validation, storage,
credential/output handling, ACME retry behavior and DNS worker lifecycle, and
reviewed the resulting fixes. The original checkout was not modified.

## Findings fixed

1. **Medium: ACME retries could bypass an oversized authority cooldown within an
   attempt.** The durable retry parser already handled integer overflow, but
   x/crypto/acme separately parsed the original header using duration arithmetic.
   An oversized `Retry-After` caused repeated requests despite the persisted
   deadline. The transport now observes the original header first, then supplies
   a rounded-up HTTP date to the library. This reuses the bounded parser and
   preserves normal bad-nonce recovery and asynchronous order polling. Tests
   cover both error responses and successful responses requiring another poll.

2. **Medium: cancellation during DNS creation destroyed cleanup state.** Killing
   the provider worker immediately could leave a remotely created TXT record
   behind even when the SDK could respond to cancellation. A private control
   pipe now cancels the active SDK context. A responsive worker retains the same
   provider instance for cleanup; an unresponsive worker is still killed and
   reaped after bounded cancellation handling. Monotonic request IDs prevent an
   early or delayed cancellation from cancelling the subsequent cleanup.
   Record creation and cleanup remain serialized. Tests cover partial creation,
   cooperative cancellation, a deliberately uncooperative provider, cancellation
   ordering, and worker exit on EOF/malformed input with a blocked control reader.

3. **Medium: shutdown could keep application listeners open during slow DNS
   cleanup.** The daemon previously joined certificate workers before draining
   listeners, potentially continuing to accept traffic for several minutes.
   `Stop` now initiates cancellation; `Close` joins after application shutdown.
   The final "stopped" log also follows that join. A regression holds cleanup
   open, verifies both application listeners stop accepting connections, and
   confirms shutdown waits for cleanup before returning.

4. **Medium: systemd could kill the provider before cleanup.** The default
   `KillMode=control-group` sends the initial termination signal to the daemon
   and worker together. The packaged service now uses `KillMode=mixed`, so the
   daemon receives SIGTERM first and can finish bounded cleanup. The existing
   six-minute stop timeout remains. This behavior follows the
   [systemd kill-mode contract](https://www.freedesktop.org/software/systemd/man/latest/systemd.kill.html).

The review also simplified subprocess output handling: nil stdout/stderr go
directly to `/dev/null`, avoiding unnecessary copying goroutines, pipes and
`WaitDelay`. Reply and cancellation descriptors are close-on-exec. The inherited
cancellation descriptor is registered with Go's poller so closing it reliably
unblocks its reader on worker exit.

## Validation

Go checks used Go 1.27.3, cached modules and bounded build concurrency (`-p 2`).

| Check | Result |
| --- | --- |
| Race tests for ACME, DNS providers, server TLS, admin, daemon and observability | Passed; local Pebble HTTP-01/DNS-01 issuance and renewal enabled. |
| Final daemon and DNS-provider race tests after review follow-ups | Passed. |
| Standard and `e2ehooks` vet/lint | Passed. |
| `e2ehooks` daemon tests | Passed. |
| DNS-provider tests in a non-root container | Passed with network disabled, read-only root filesystem, all capabilities dropped and no-new-privileges; included cancellation, forced termination, Cloudflare/RFC2136 local fixtures and output redaction. |
| Independent reviewer EOF/malformed-input race reproductions | Passed; permanent regression coverage included. |
| systemd unit syntax | Passed with only `ExecStart` replaced by a locally installed harmless binary; the host does not have the packaged daemon path. No live systemd service was changed. |
| Diff whitespace | Passed. |

No additional must-fix issue was identified during the independent follow-up
review. No public CA, provider account or public DNS record was used. As before,
cleanup after a crash, uncooperative SDK, or remote API failure remains best
effort; the provider process has the daemon's privileges and is not a security
sandbox. Live interoperability with every bundled DNS provider is not claimed.

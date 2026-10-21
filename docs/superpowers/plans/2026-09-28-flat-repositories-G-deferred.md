# Flat repositories — phase G deferred minors

Phase G's sixteen task reviews and its fix round (2026-10-21) found these issues and triaged them "can wait". None changes the bytes a client is served: GD-1 is a bounded wait phase G introduced, which the Task 10 review asked to revisit; GD-2 a pre-existing mismatch between `ParseSources` and spec C5's wording; GD-3 a test the reviewer marked optional. They are numbered `GD-n` (ruling PG-25), never `G-n`, which is spec §10's security-review ID (`G-1`).

**How they get done:** every later phase plan includes the items below that sit in files it already touches (cite the item ID in the task); an owning phase in parentheses marks where an item most naturally lands. Phase H ends with a sweep task that verifies each remaining item against the code, fixes it or records why not, and deletes this file.

Left out: everything the fix round resolved — every task review's Important and Minor findings but the three below — and what is no item: the commit-message nits (history is not rewritten), the findings the fix round showed no single-site mutant can reach (their assertions stay as documentation), and the Task 16 review's Minor 8 (`adoption_member_skipped` and `snapshot_index_target_404` are undocumented in `docs/log-fields.md`), which is an H carry-forward in the overview. The phase G final review adds its own items here. Line numbers are as of the fix round and will drift.

## internal/freshness — the anchorless refresh

- **GD-1** `internal/freshness/anchorless.go` (`refreshAnchorlessRow`'s wait for a body slot): an anchorless check waits for one of the two process-wide body slots (`anchorlessBodySlots`) while it holds its host's slot (`Config.HostLimiter`, the `hostsem.Sem` the handler's client misses share), and a slow request keeps its body slot until `checkDeadline` (2 min; the fetch transport has no response-header timeout). Two slow anchorless requests anywhere can so make other hosts' anchorless checks hold a host slot idle up to their deadlines. No deadlock (a body-slot holder never waits for a host slot); bounded; failed checks back off; `TestCheck_AnchorlessSlotWaitEndsWithTheCheck` pins that the wait ends with the check. Follow-up: a short bounded wait for a body slot (unknown when it expires), or a per-request timeout well under `checkDeadline`. The AIDEV-NOTE at the slot wait points here. (H) (Task 10 review M1, ruling PG-24)

## internal/freshness — Sources Filenames

- **GD-2** `internal/freshness/sources_parse.go` (`ParseSources`): it checks each stanza's `Directory` and each `Checksums-Sha256` file path with `validateMemberPath`, although spec C5 says `ParsePackages` uses `validateFilename` for "Packages/Sources Filenames". Harmless for flat suites (`buildSourceHashes` builds nothing for them, C6). A `dists/` repository whose `Sources` names a `Directory` or a file with a `%` or a `.` segment loses that stanza's or that file's source rows silently — no WARN: `source_parse_failed` is logged only for a whole file that fails (pre-existing). Align `ParseSources` with C5, or correct C5's parenthetical to "Packages Filenames". (H) (Task 3 review Minor 3, ruling PG-21)

## Tests (the whole-branch review)

- **GD-3** `internal/handler/flat_dod_test.go` (`TestServeHTTP_MintedFlatSuitesAtScale`): the flat minted-suites simulation refuses at both caps but never evicts, while phase D's sibling does; twin eviction is pinned only at the cache layer (`TestCreateSuite_EvictionLeavesTheOtherSchemesTwin` and its table, `internal/cache/suite_evict_test.go:258`). The reviewer marked it optional and acceptable; an eviction phase must keep the simulation under the 4.5 s `-race` budget (ruling R-G5). The phase G final review may reopen it. (H) (Task 15 review Minor 4, ruling PG-26)

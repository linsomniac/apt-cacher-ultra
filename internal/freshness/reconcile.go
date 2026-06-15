package freshness

import "github.com/linsomniac/apt-cacher-ultra/internal/cache"

// missingRequestableMembers returns the declared ReleaseMembers belonging
// to a requestable IndexTarget GROUP that the snapshot does not currently
// serve. It reuses missingRequestableIndexGroups (the Layer-2 serve-contract
// predicate) for the group decision — "all" always required, allowlisted
// arches when filtered — then returns every declared variant of each missing
// group so apt can fetch whichever it asks for.
//
// AIDEV-NOTE: The conversion from []cache.SnapshotMember to []ReleaseMember
// is needed because missingRequestableIndexGroups only inspects m.Path, so
// only Path is populated in the synthetic presentMembers slice. This is
// safe because missingRequestableIndexGroups only calls indexTargetGroup(m.Path).
func missingRequestableMembers(declared []ReleaseMember, present []cache.SnapshotMember, allowlist map[string]struct{}) []ReleaseMember {
	presentMembers := make([]ReleaseMember, len(present))
	for i, p := range present {
		presentMembers[i] = ReleaseMember{Path: p.Path}
	}
	missingGroups := missingRequestableIndexGroups(declared, presentMembers, allowlist)
	if len(missingGroups) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(missingGroups))
	for _, g := range missingGroups {
		want[g] = struct{}{}
	}
	var out []ReleaseMember
	for _, m := range declared {
		if group, _, ok := indexTargetGroup(m.Path); ok {
			if _, missing := want[group]; missing {
				out = append(out, m)
			}
		}
	}
	return out
}

package freshness

import (
	"reflect"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

func TestMissingRequestableMembers(t *testing.T) {
	rm := func(path, sha string) ReleaseMember { return ReleaseMember{Path: path, SHA256: sha} }
	declared := []ReleaseMember{
		rm("main/binary-amd64/Packages", "a"),
		rm("main/binary-all/Packages", "b"),
		rm("main/binary-all/Packages.gz", "c"),
		rm("main/binary-arm64/Packages", "d"), // foreign, allowlisted-out
	}
	present := []cache.SnapshotMember{{Path: "main/binary-amd64/Packages"}}
	allow := map[string]struct{}{"amd64": {}}

	got := missingRequestableMembers(declared, present, allow)
	want := []ReleaseMember{
		rm("main/binary-all/Packages", "b"),
		rm("main/binary-all/Packages.gz", "c"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("missingRequestableMembers() = %v, want %v", got, want)
	}
}

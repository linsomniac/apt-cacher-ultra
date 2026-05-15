package gpg

import (
	"embed"
	"io/fs"
	"sort"
)

//go:embed embedded/*.gpg
var embeddedFS embed.FS

// EmbeddedSource is one in-binary keyring file. Name is the bare file
// name (without the "embedded/" prefix) and is used as the source
// attribution string ("embedded:<name>") on every loaded entity.
type EmbeddedSource struct {
	Name string
	Data []byte
}

// BundledSources returns the keyring files compiled into the binary
// (canonical Ubuntu, Debian, and Ubuntu Pro ESM archive keys). The
// slice is sorted by Name for deterministic load order.
//
// Operators can supplement these with on-disk keyrings via the
// adoption.keyring_dirs config setting; on-disk keys for the same
// primary fingerprint take precedence (first-seen wins, and on-disk
// dirs are loaded before embedded sources).
//
// Refresh the bundled .gpg files with scripts/refresh-embedded-keys.sh.
func BundledSources() []EmbeddedSource {
	entries, err := fs.ReadDir(embeddedFS, "embedded")
	if err != nil {
		// embed.FS reads from a static archive baked into the binary;
		// a ReadDir failure here would mean the build itself was
		// corrupt — return nothing rather than panicking on import.
		return nil
	}
	out := make([]EmbeddedSource, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := embeddedFS.ReadFile("embedded/" + e.Name())
		if err != nil {
			continue
		}
		out = append(out, EmbeddedSource{Name: e.Name(), Data: data})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

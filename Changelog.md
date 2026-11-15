# Changelog

## 1.0.5 — 2026-11-15

- Backport verified reconstruction of uncompressed `Packages` and `Sources`
  indexes for [#32](https://github.com/linsomniac/apt-cacher-ultra/issues/32).
  Repositories that serve gzip/xz bytes at a plain index URL no longer block
  adoption when a declared compressed sibling reconstructs the exact index.
- Check the compressed and reconstructed files against their Release-declared
  sizes and SHA-256 hashes, with bounded downloads and decompression. Recovery
  also covers reconciliation and missing cached snapshot indexes.
- No configuration change, database migration, or cache deletion is required.
  Thanks to @hraphael for the report and follow-up. See
  [the release notes](docs/release-notes/1.0.5.md).

## 1.0.4 — 2026-11-11

- Fix Debian installer mirror checks failing against repositories adopted from
  `InRelease` ([#30](https://github.com/linsomniac/apt-cacher-ultra/issues/30)).
  Serve matching `Release` and `Release.gpg` files from the cached snapshot,
  preserving its original signed content and signatures. Existing snapshots
  recover after upgrading and restarting, including offline; no cache reset,
  database migration, or configuration change is needed.
- Thank you to @charlescurley for reporting the problem. See
  [the release notes](docs/release-notes/1.0.4.md) for details and validation.

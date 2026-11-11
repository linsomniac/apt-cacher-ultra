# Changelog

## 1.0.4 — 2026-11-11

- Fix Debian installer mirror checks failing against repositories adopted from
  `InRelease` ([#30](https://github.com/linsomniac/apt-cacher-ultra/issues/30)).
  Serve matching `Release` and `Release.gpg` files from the cached snapshot,
  preserving its original signed content and signatures. Existing snapshots
  recover after upgrading and restarting, including offline; no cache reset,
  database migration, or configuration change is needed.
- Thank you to @charlescurley for reporting the problem. See
  [the release notes](docs/release-notes/1.0.4.md) for details and validation.

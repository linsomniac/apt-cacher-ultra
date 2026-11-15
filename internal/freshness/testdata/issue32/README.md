# Raspberry Pi archive regression fixture

Captured on 2026-11-15 from
https://archive.raspberrypi.com/debian/dists/trixie/main/source/Sources.gz
for https://github.com/linsomniac/apt-cacher-ultra/issues/32.
The unsuffixed Sources URL returned identical gzip bytes with
`Content-Location: Sources.gz`, reproducing the reported sizes exactly.

The same archive's InRelease declared these SHA256 entries:

```
9d688fee5dd02cf4f3b28b9d94f8d260aae7db4b95927b5b1382b037dd40720f 443932 main/source/Sources
e9ffbb3867746a0dce3633f770decef4337dfa6ccfd67379a53ab275ef54c773 123988 main/source/Sources.gz
```

The regression test replays these bytes through the real HTTP fetch client
and the adopter with its default empty architecture allowlist. It checks
member verification and snapshot publication; the test verifier bypasses
Release signature verification, which is covered separately by the GPG suite.

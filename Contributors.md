The apt-cacher-ultra project would like to thank the following for their
contributions:

Andy Smith -- suggested proxy readiness health checks and delaying the proxy listener until startup completes; reported the need for client-address logging behind proxies

Nicholas Scott (@kband)
Sam Parkes (@Sam-Parkes)
@sdh91 -- requested admin dashboard usage statistics in issue #11
@skibbipl
Venkat (@venkatamutyala) -- flat repository support is based on their PR #8
Laurent Corbes (@lcaflc) -- admin TLS and ACME support is based on their PRs #20/#21; empty repository index handling is based on their PR #22
@grifferz -- requested X-Forwarded-For support in issue #15
Scott Hyndman (@shyndman) -- NUL-padded gzip index support in PR #26; prepared statement reuse for faster snapshot adoption in PR #28
@charlescurley -- reported source-index failures in issue #29, prompting source-index recovery fixes and interrupted metadata download regression coverage

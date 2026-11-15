"""Optional larger publication burst; real signed metadata and APT clients."""
import email.utils
import gzip
import hashlib
import json
import lzma
import os
from pathlib import Path
import sqlite3
import subprocess
import time

from lab import Lab, WORK, RESULTS, BASE, MIRROR, SUITE


class BurstLab(Lab):
    def __init__(self, args):
        super().__init__(args)
        self.measure_delays = True
        self.suites = [SUITE] + [f"suite-{i:02d}" for i in range(1, args.suites)]
        self.hot = [f"acu-burst-{i:02d}-{j:02d}" for i in range(args.suites) for j in range(args.hot_count)]

    def host(self, i):
        return "repo.test" if i % self.args.hosts == 0 else "repo2.test"

    def prefix(self, i):
        return MIRROR if self.host(i) == "repo.test" else "/burst/repo2/debian-security"

    def build_repo(self, version):
        root = WORK / "repos" / version
        for i, suite in enumerate(self.suites):
            stanzas = []
            for j in range(self.args.hot_count):
                name = f"acu-burst-{i:02d}-{j:02d}"
                pkgroot = WORK / "packages" / version / name
                (pkgroot / "DEBIAN").mkdir(parents=True)
                (pkgroot / f"usr/share/{name}").mkdir(parents=True)
                control = (f"Package: {name}\nVersion: {version}\nArchitecture: amd64\n"
                           "Maintainer: ACU Lab <lab@example.invalid>\nDescription: burst probe\n")
                (pkgroot / "DEBIAN/control").write_text(control)
                (pkgroot / f"usr/share/{name}/version").write_text(version)
                # Deterministic, poorly compressible payload, distinct per suite/version.
                payload = b"".join(hashlib.sha256(f"{name}/{version}/{k}".encode()).digest() for k in range(4096))
                (pkgroot / f"usr/share/{name}/payload").write_bytes(payload)
                deb = root / f"pool/main/a/{name}/{name}_{version}_amd64.deb"
                deb.parent.mkdir(parents=True)
                self.command(["dpkg-deb", "--build", str(pkgroot), str(deb)], "setup.log")
                body = deb.read_bytes()
                stanzas.append(control + f"Filename: {deb.relative_to(root)}\nSize: {len(body)}\nSHA256: {hashlib.sha256(body).hexdigest()}\n\n")
            # Catalog rows exercise parsing/storage; clients request only the real
            # hot fixtures above. These filler artifacts are deliberately not served.
            for j in range(self.args.catalog_size):
                name = f"catalog-{i:02d}-{j:06d}"
                digest = hashlib.sha256(f"{name}/{version}".encode()).hexdigest()
                stanzas.append(f"Package: {name}\nVersion: {version}\nArchitecture: amd64\n"
                               f"Filename: pool/main/c/{name}/{name}_{version}_amd64.deb\nSize: 1024\nSHA256: {digest}\n"
                               f"Description: catalog fixture {digest}\n\n")
            data = "".join(stanzas).encode()
            dists = root / "dists" / suite
            # Twelve indexes: two architectures and two components, three encodings.
            for component in ("main", "contrib"):
                for arch in ("amd64", "arm64"):
                    index = data if arch == "amd64" else data.replace(b"amd64", b"arm64")
                    directory = dists / component / f"binary-{arch}"
                    directory.mkdir(parents=True)
                    (directory / "Packages").write_bytes(index)
                    (directory / "Packages.gz").write_bytes(gzip.compress(index, mtime=0))
                    (directory / "Packages.xz").write_bytes(lzma.compress(index))
        self.versions[version] = root

    def publish(self, version):
        while int(time.time()) <= self.published_at:
            time.sleep(0.05)
        stamp = int(time.time())
        root = self.versions[version]
        hashes = {}
        for suite in self.suites:
            dists = root / "dists" / suite
            release = (f"Origin: ACU burst lab\nLabel: ACU burst\nSuite: {suite}\nCodename: {suite}\n"
                       f"Date: {email.utils.formatdate(stamp, usegmt=True)}\nArchitectures: amd64 arm64\n"
                       f"Components: main contrib\nDescription: generation {version}\nSHA256:\n")
            for member in sorted(dists.rglob("Packages*")):
                body = member.read_bytes()
                release += f" {hashlib.sha256(body).hexdigest()} {len(body)} {member.relative_to(dists)}\n"
            (dists / "Release").write_text(release)
            self.command(self.gpg + ["--digest-algo", "SHA256", "--clearsign", "--output", str(dists / "InRelease"), str(dists / "Release")], "setup.log")
            hashes[suite] = hashlib.sha256((dists / "InRelease").read_bytes()).hexdigest()
        for path in root.rglob("*"):
            if path.is_file():
                os.utime(path, (stamp, stamp))
        self.published_at, self.active = stamp, version
        return self.event("publish", version=version, origin_sha256=hashes)

    def write_config(self):
        super().write_config()
        path = RESULTS / "config.toml"
        text = path.read_text().replace('"^repo[.]test$"', '"^repo[.]test$", "^repo2[.]test$"')
        text += '\n[[mirror]]\nprefix = "/burst/repo2/debian-security"\nupstream = "http://repo2.test/debian-security/"\n'
        path.write_text(text)

    def start_origin(self):
        Path("/etc/apt/sources.list").write_text("".join(
            f"deb [signed-by={WORK}/keys/lab.gpg] http://127.0.0.1:3142{self.prefix(i)} {s} main contrib\n"
            for i, s in enumerate(self.suites)))
        super().start_origin()

    def origin_delay(self, path, generation):
        if generation != "2.0" or not self.measure_delays:
            return 0
        if path.endswith("_2.0_amd64.deb"):
            return self.args.deb_delay
        if "/dists/" in path and "Packages" in path:
            return self.args.member_delay
        return 0

    def current_snapshots(self):
        with sqlite3.connect(f"file:{WORK}/cache/cache.db?mode=ro", uri=True, timeout=1) as conn:
            return {row[0].rsplit("/", 1)[-1]: (row[1], row[2]) for row in conn.execute(
                "SELECT f.suite_path, s.snapshot_id, s.inrelease_hash FROM suite_freshness f "
                "JOIN suite_snapshot s ON s.snapshot_id=f.current_snapshot_id")}

    def client_update(self, phase):
        self.round += 1
        logfile = f"apt-{self.round:03d}.log"
        start = time.monotonic()
        self.command(["apt-get", "update"], logfile, timeout=180)
        policy = subprocess.check_output(["apt-cache", "policy", *self.hot], text=True)
        with (RESULTS / logfile).open("a") as output:
            output.write(policy)
        candidates = [line.split(":", 1)[1].strip() for line in policy.splitlines() if "Candidate:" in line]
        candidate = candidates[0] if len(candidates) == len(self.hot) and len(set(candidates)) == 1 else "mixed"
        current = self.current_snapshots()
        row = self.event("client_observation", phase=phase, round=self.round, candidate=candidate,
                         snapshot="all" if len(current) == len(self.suites) else None,
                         snapshots=current, apt_seconds=round(time.monotonic()-start, 6))
        self.observations.append(row)
        return row

    def install(self, version):
        # As in the single-suite lab, separate publication from on-demand
        # downloads: release the injected fault before the final install.
        if version == "2.0":
            self.measure_delays = False
        self.command(["apt-get", "install", "-y", "--no-install-recommends", *[f"{n}={version}" for n in self.hot]],
                     f"install-{version}.log", timeout=240)
        for name in self.hot:
            got = subprocess.check_output(["dpkg-query", "-W", "-f=${Version}", name], text=True)
            if got != version or Path(f"/usr/share/{name}/version").read_text() != version:
                raise RuntimeError(f"wrong installed version: {name}: {got}")
        self.event("installed", version=version, count=len(self.hot))

    def wait_version(self, version, phase, require_snapshot=True):
        if phase != "measured" or not self.args.periodic_only:
            return super().wait_version(version, phase, require_snapshot)
        # Read-only DB observation: do not fire T1 through cache GETs while
        # measuring background-only detection and publication.
        expected = {s: hashlib.sha256((self.versions[version]/"dists"/s/"InRelease").read_bytes()).hexdigest() for s in self.suites}
        deadline = time.monotonic()+self.args.max_wait
        while time.monotonic() < deadline:
            current = self.current_snapshots()
            if all(s in current and current[s][1] == digest for s, digest in expected.items()):
                return super().wait_version(version, phase, require_snapshot)
            time.sleep(1)
        raise RuntimeError("background-only publication did not finish")

    def summarize(self, publication, visible):
        suites = {}
        for row in list(self.events):
            rec = row.get("record", {})
            if row["t"] < publication["t"] or rec.get("msg") not in ("freshness_check", "adoption_started", "adoption_success", "adoption_finished"):
                continue
            suite = rec.get("suite_path", "").rsplit("/", 1)[-1]
            if suite not in self.suites:
                continue
            stats = suites.setdefault(suite, {})
            event = rec["msg"]
            if event == "freshness_check" and rec.get("result") not in ("changed", "changed_queued", "changed_deferred"):
                continue
            if event != "adoption_finished":
                stats.setdefault(event+"_seconds", round(row["t"]-publication["t"], 6))
            if event == "adoption_finished":
                stats.setdefault("attempts", []).append({"seconds": round(row["t"]-publication["t"], 6), "outcome": rec.get("outcome"), "adoption_id": rec.get("adoption_id")})
                stats["adoption_finished_seconds"] = round(row["t"]-publication["t"], 6)
                stats["first_seen_seconds"] = rec.get("first_seen_seconds")
                stats["release_age_seconds"] = rec.get("release_age_seconds")
                stats["hot"] = {k: rec[k] for k in ("hot_count", "hot_fetched", "hot_failed", "hot_unattempted") if k in rec}
                stats["phases"] = rec.get("phase_seconds")
                stats["outcome"] = rec.get("outcome")
        if len(suites) != len(self.suites) or any("adoption_success_seconds" not in s for s in suites.values()):
            raise RuntimeError(f"missing measured publication: {suites}")
        summary = dict(scenario="burst", options=vars(self.args), suites=suites,
                       publication_to_client_seconds=visible["t"]-publication["t"],
                       slowest_publication_seconds=max(s["adoption_success_seconds"] for s in suites.values()),
                       caveat="Synthetic burst with larger catalogs, not a full Debian/Ubuntu mirror or an SLA.")
        (RESULTS/"summary.json").write_text(json.dumps(summary, indent=2)+"\n")
        self.event("summary", **summary)

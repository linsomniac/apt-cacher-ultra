#!/usr/bin/env python3
"""Measure a real APT client's path from signed publication to installation."""
import argparse
import email.utils
import gzip
import hashlib
import http.server
import json
import os
from pathlib import Path
import shutil
import socket
import sqlite3
import subprocess
import threading
import time
import urllib.error
import urllib.request


PKG = "acu-lab-php8.4"
SUITE = "trixie-security"
BASE = "/debian-security"
MIRROR = "/debian/security.debian.org/debian-security"
ANCHOR = f"{BASE}/dists/{SUITE}/InRelease"
OPTIONAL = f"{BASE}/dists/{SUITE}/main/dep11/Components-amd64.yml.gz"
RESULTS = Path("/results")
WORK = Path("/tmp/acu-freshness-lab")


class Lab:
    def __init__(self, args):
        self.args = args
        self.start = time.monotonic()
        self.lock = threading.Lock()
        self.active = "1.0"
        self.published_at = 0
        self.fault = "none"
        self.blocked = threading.Event()
        self.server = None
        self.daemon = None
        self.round = 0
        self.observations = []
        self.events = []
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def event(self, kind, **fields):
        row = dict(t=round(time.monotonic() - self.start, 6),
                   unix=time.time(), event=kind, **fields)
        with self.lock:
            self.events.append(row)
            with (RESULTS / "timeline.jsonl").open("a") as f:
                f.write(json.dumps(row) + "\n")
        if kind not in ("origin_response", "origin_request", "acu_log"):
            print(json.dumps(row), flush=True)
        return row

    def command(self, argv, logfile, timeout=120):
        with (RESULTS / logfile).open("a") as f:
            f.write("\n$ " + " ".join(map(str, argv)) + "\n")
            f.flush()
            result = subprocess.run(argv, stdout=f, stderr=subprocess.STDOUT,
                                    timeout=timeout, check=False)
        if result.returncode:
            raise RuntimeError(f"Command failed ({result.returncode}); see {logfile}")

    def setup(self):
        WORK.mkdir()
        (WORK / "keys").mkdir()
        (WORK / "cache").mkdir()
        keyhome = WORK / "gnupg"
        keyhome.mkdir(mode=0o700)
        self.gpg = ["gpg", "--homedir", str(keyhome), "--batch", "--yes",
                    "--pinentry-mode", "loopback", "--passphrase", ""]
        self.command(self.gpg + ["--quick-generate-key", "ACU freshness lab <lab@example.invalid>",
                                "rsa2048", "sign", "0"], "setup.log")
        self.command(self.gpg + ["--output", str(WORK / "keys/lab.gpg"), "--export"], "setup.log")
        self.versions = {}
        for version in ("1.0", "1.1", "2.0"):
            self.build_repo(version)
        self.write_config()
        os.chmod(WORK, 0o755)
        Path("/etc/apt/sources.list").write_text(
            f"deb [signed-by={WORK}/keys/lab.gpg] http://127.0.0.1:3142{MIRROR} {SUITE} main\n")
        Path("/etc/apt/apt.conf.d/99freshness-lab").write_text('''
Acquire::Languages "none";
Acquire::Retries "0";
Acquire::http::Timeout "20";
APT::Update::Error-Mode "any";
''')
        # Container-only state. Keep the client list files between updates.
        self.event("environment", scenario=self.args.scenario,
                   binary_sha256=hashlib.sha256(Path("/lab/acu").read_bytes()).hexdigest(),
                   binary_version=subprocess.check_output(["/lab/acu", "-version"], text=True).strip(),
                   apt_version=subprocess.check_output(["apt-get", "--version"], text=True).splitlines()[0],
                   options=vars(self.args))
        self.start_origin()
        self.publish("1.0")
        self.daemon = subprocess.Popen(["/lab/acu", "-config", str(RESULTS / "config.toml")],
                                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                       text=True, bufsize=1)
        self.log_thread = threading.Thread(target=self.capture_daemon, daemon=True)
        self.log_thread.start()
        until = time.monotonic() + 30
        while time.monotonic() < until:
            if self.daemon.poll() is not None:
                raise RuntimeError("ACU exited; see acu.jsonl")
            try:
                with socket.create_connection(("127.0.0.1", 3142), timeout=1):
                    return
            except OSError:
                time.sleep(0.1)
        raise RuntimeError("ACU listener did not start")

    def capture_daemon(self):
        # ACU 1.0.2's JSON logger omits timestamps. Timestamp its output in the
        # harness rather than silently assuming each record has a `time` key.
        with (RESULTS / "acu.jsonl").open("w") as output:
            for line in self.daemon.stdout:
                output.write(line)
                output.flush()
                try:
                    record = json.loads(line)
                except json.JSONDecodeError:
                    record = {"raw": line.rstrip()}
                self.event("acu_log", record=record)

    def build_repo(self, version):
        root = WORK / "repos" / version
        pkgroot = WORK / "packages" / version
        (pkgroot / "DEBIAN").mkdir(parents=True)
        (pkgroot / "usr/share/acu-lab").mkdir(parents=True)
        (pkgroot / "DEBIAN/control").write_text(
            f"Package: {PKG}\nVersion: {version}\nArchitecture: amd64\n"
            "Maintainer: ACU Lab <lab@example.invalid>\nDescription: disposable freshness probe\n")
        (pkgroot / "usr/share/acu-lab/version").write_text(version + "\n")
        pool = root / "pool/main/a" / PKG
        pool.mkdir(parents=True)
        deb = pool / f"{PKG}_{version}_amd64.deb"
        self.command(["dpkg-deb", "--build", str(pkgroot), str(deb)], "setup.log")
        contents = deb.read_bytes()
        package_index = (
            (pkgroot / "DEBIAN/control").read_text() +
            f"Filename: {deb.relative_to(root)}\nSize: {len(contents)}\n"
            f"SHA256: {hashlib.sha256(contents).hexdigest()}\n\n").encode()
        dists = root / "dists" / SUITE
        indexes = dists / "main/binary-amd64"
        indexes.mkdir(parents=True)
        (indexes / "Packages").write_bytes(package_index)
        (indexes / "Packages.gz").write_bytes(gzip.compress(package_index, mtime=0))
        if self.args.scenario == "busy-repair":
            optional = dists / "main/dep11/Components-amd64.yml.gz"
            optional.parent.mkdir(parents=True)
            optional.write_bytes(gzip.compress(b"File: DEP-11\nVersion: '0.16'\n", mtime=0))
        self.versions[version] = root

    def publish(self, version):
        # Avoid accidental same-second Last-Modified ambiguity in the baseline.
        while int(time.time()) <= self.published_at:
            time.sleep(0.05)
        stamp = int(time.time())
        root = self.versions[version]
        dists = root / "dists" / SUITE
        members = sorted(p for p in dists.rglob("*") if p.is_file() and p.name not in ("Release", "InRelease"))
        release = (f"Origin: ACU freshness lab\nLabel: ACU lab\nSuite: {SUITE}\nCodename: {SUITE}\n"
                   f"Date: {email.utils.formatdate(stamp, usegmt=True)}\n"
                   f"Architectures: amd64\nComponents: main\nDescription: generation {version}\nSHA256:\n")
        for member in members:
            data = member.read_bytes()
            release += f" {hashlib.sha256(data).hexdigest()} {len(data)} {member.relative_to(dists)}\n"
        (dists / "Release").write_text(release)
        self.command(self.gpg + ["--digest-algo", "SHA256", "--clearsign", "--output",
                                str(dists / "InRelease"), str(dists / "Release")], "setup.log")
        for p in root.rglob("*"):
            if p.is_file():
                os.utime(p, (stamp, stamp))
        # The serving thread reads this immutable generation in one assignment.
        self.published_at = stamp
        self.active = version
        return self.event("publish", version=version,
                          origin_sha256=hashlib.sha256((dists / "InRelease").read_bytes()).hexdigest())

    def write_config(self):
        # Defaults retained unless a knob is explicitly set by this experiment.
        retries = 0 if self.args.scenario == "busy-repair" else 2
        text = f'''[cache]
dir = "{WORK}/cache"
listen = "127.0.0.1:3142"
[upstream]
allowed_host_regex = ["^repo[.]test$"]
deny_target_ranges = []
[freshness]
cooldown = "{self.args.cooldown}"
{f'periodic_refresh = "{self.args.refresh}"' if self.args.refresh else ""}
max_concurrent_adoptions = 2
[adoption]
enabled = true
keyring_dirs = ["{WORK}/keys"]
{f'hot_prefetch_budget = "{self.args.prefetch_budget}"' if self.args.prefetch_budget else ""}
member_retry_count = {retries}
repair_skipped_members = true
[hot_packages]
window = "24h"
[gc]
enabled = false
[integrity]
validate_at_rest_interval = "0s"
[admin]
enabled = false
[log]
level = "info"
format = "json"
[[mirror]]
prefix = "{MIRROR}"
upstream = "http://repo.test{BASE}/"
'''
        (RESULTS / "config.toml").write_text(text)

    def start_origin(self):
        lab = self

        class Origin(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                begin = lab.event("origin_request", path=self.path,
                                  if_none_match=self.headers.get("If-None-Match"),
                                  if_modified_since=self.headers.get("If-Modified-Since"))
                generation = lab.active
                relative = self.path.removeprefix(BASE + "/")
                path = lab.versions[generation] / relative
                if self.path.startswith(BASE + "/pool/"):
                    for root in lab.versions.values():
                        if (root / relative).is_file():
                            path = root / relative
                            break
                if not self.path.startswith(BASE + "/") or ".." in Path(relative).parts or not path.is_file():
                    self.send_error(404)
                    lab.event("origin_response", path=self.path, status=404, generation=generation)
                    return
                data = path.read_bytes()
                if lab.fault == "bad-optional" and self.path == OPTIONAL:
                    data = b"wrong metadata"
                should_delay = ((lab.fault == "slow-deb" and self.path.endswith("_2.0_amd64.deb")) or
                                (lab.fault == "repair" and self.path == OPTIONAL))
                if should_delay:
                    lab.event("fault_delay_started", path=self.path, seconds=lab.args.delay)
                    lab.blocked.set()
                    time.sleep(lab.args.delay)
                extra_delay = lab.origin_delay(self.path, generation)
                if extra_delay:
                    time.sleep(extra_delay)
                etag = '"' + hashlib.sha256(data).hexdigest() + '"'
                stamp = int(path.stat().st_mtime)
                unchanged = self.headers.get("If-None-Match") == etag
                if not self.headers.get("If-None-Match") and self.headers.get("If-Modified-Since"):
                    try:
                        unchanged = email.utils.parsedate_to_datetime(self.headers["If-Modified-Since"]).timestamp() >= stamp
                    except (ValueError, TypeError):
                        pass
                status = 304 if unchanged else 200
                self.send_response(status)
                self.send_header("ETag", etag)
                self.send_header("Last-Modified", email.utils.formatdate(stamp, usegmt=True))
                if status == 200:
                    self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                try:
                    if status == 200:
                        self.wfile.write(data)
                except (BrokenPipeError, ConnectionResetError):
                    status = "client_disconnected"
                lab.event("origin_response", path=self.path, status=status, generation=generation,
                          seconds=round(time.monotonic() - lab.start - begin["t"], 6))

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 80), Origin)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def origin_delay(self, path, generation):
        return 0

    def get_anchor(self):
        url = f"http://127.0.0.1:3142{MIRROR}/dists/{SUITE}/InRelease"
        with self.opener.open(url, timeout=20) as r:
            return dict(cached_sha256=hashlib.sha256(r.read()).hexdigest(),
                        snapshot=r.headers.get("X-Cache-Snapshot"),
                        last_modified=r.headers.get("Last-Modified"))

    def database_state(self):
        db = WORK / "cache/cache.db"
        try:
            with sqlite3.connect(f"file:{db}?mode=ro", uri=True, timeout=1) as conn:
                conn.row_factory = sqlite3.Row
                row = conn.execute("SELECT current_snapshot_id, last_check_at, last_success_at, "
                                   "inrelease_change_seen_at FROM suite_freshness LIMIT 1").fetchone()
                return dict(row) if row else {}
        except sqlite3.Error as e:
            return {"read_error": str(e)}

    def client_update(self, phase):
        self.round += 1
        log = f"apt-{self.round:03d}.log"
        before = time.monotonic()
        self.command(["apt-get", "update"], log, timeout=90)
        policy = subprocess.check_output(["apt-cache", "policy", PKG], text=True)
        with (RESULTS / log).open("a") as f:
            f.write(policy)
        candidate = next((line.split(":", 1)[1].strip() for line in policy.splitlines()
                          if "Candidate:" in line), None)
        # This diagnostic GET can trigger freshness too, so it is logged explicitly.
        anchor = self.get_anchor()
        direct = (self.versions[self.active] / "dists" / SUITE / "InRelease").read_bytes()
        row = self.event("client_observation", phase=phase, round=self.round, candidate=candidate,
                         apt_seconds=round(time.monotonic() - before, 6),
                         origin_sha256=hashlib.sha256(direct).hexdigest(),
                         database=self.database_state(), **anchor)
        self.observations.append(row)
        return row

    def install(self, version):
        self.command(["apt-get", "install", "-y", "--no-install-recommends", f"{PKG}={version}"],
                     f"install-{version}.log", timeout=max(120, self.args.delay + 60))
        got = subprocess.check_output(["dpkg-query", "-W", "-f=${Version}", PKG], text=True)
        payload = Path("/usr/share/acu-lab/version").read_text().strip()
        if got != version or payload != version:
            raise RuntimeError(f"Installed version or payload is wrong: {got}, {payload}")
        self.event("installed", version=got)

    def wait_version(self, version, phase, require_snapshot=True):
        deadline = time.monotonic() + self.args.max_wait
        while time.monotonic() < deadline:
            row = self.client_update(phase)
            if row["candidate"] == version and (row["snapshot"] or not require_snapshot):
                return row
            time.sleep(self.args.client_interval)
        raise RuntimeError(f"Client did not see {version} within {self.args.max_wait}s")

    def run(self):
        self.setup()
        if self.args.scenario == "busy-repair":
            self.fault = "bad-optional"
        self.wait_version("1.0", "initial", require_snapshot=False)
        self.install("1.0")
        # 1.0.2 adopts only on changed content; an explicit second generation
        # establishes an adopted, hot baseline before the measured update.
        self.publish("1.1")
        self.wait_version("1.1", "bootstrap")
        self.install("1.1")
        if self.args.scenario == "busy-repair":
            self.fault = "repair"
            until = time.monotonic() + self.args.max_wait
            while not self.blocked.is_set() and time.monotonic() < until:
                self.client_update("await_repair")
                self.blocked.wait(self.args.client_interval)
            if not self.blocked.is_set():
                raise RuntimeError("Repair did not start; cannot test busy repair")
        elif self.args.scenario == "slow-prefetch":
            self.fault = "slow-deb"
        publication = self.publish("2.0")
        visible = self.wait_version("2.0", "measured")
        self.fault = "none"
        self.install("2.0")
        self.summarize(publication, visible)

    def summarize(self, publication, visible):
        phases = {}
        for row in list(self.events):
            if row["event"] != "acu_log" or row["t"] < publication["t"]:
                continue
            msg = row["record"].get("msg", "")
            if msg in ("InRelease changed at upstream", "adoption_hot_prefetch_started", "adoption_success"):
                phases.setdefault(msg, round(row["t"] - publication["t"], 6))
        summary = dict(scenario=self.args.scenario, publication_to_client_seconds=round(visible["t"] - publication["t"], 6),
                       phases_seconds_after_publication=phases,
                       old_client_observations=sum(r["phase"] == "measured" and r["candidate"] != "2.0"
                                                   for r in self.observations),
                       faults=[r for r in self.events if r["event"] == "fault_delay_started"],
                       caveat="Synthetic mechanism reproduction, not proof of the customer's root cause.")
        (RESULTS / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        self.event("summary", **summary)

    def close(self):
        if self.daemon is not None and self.daemon.poll() is None:
            self.daemon.terminate()
            try:
                self.daemon.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.daemon.kill()
                self.daemon.wait()
        if self.server is not None:
            self.server.shutdown()
        if hasattr(self, "log_thread"):
            self.log_thread.join(timeout=5)
        if (WORK / "cache/cache.db").exists():
            shutil.copytree(WORK / "cache", RESULTS / "cache", dirs_exist_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scenario", choices=("baseline", "slow-prefetch", "busy-repair"), default="baseline")
    parser.add_argument("--cooldown", default="60s")
    parser.add_argument("--refresh", help="omit to use the binary default")
    parser.add_argument("--prefetch-budget", help="omit to use the binary default")
    parser.add_argument("--delay", type=float, default=90)
    parser.add_argument("--client-interval", type=float, default=5)
    parser.add_argument("--max-wait", type=float, default=600)
    parser.add_argument("--burst", action="store_true", help="measure simultaneous multi-suite publications")
    parser.add_argument("--suites", type=int, default=6)
    parser.add_argument("--hosts", type=int, choices=(1, 2), default=2)
    parser.add_argument("--catalog-size", type=int, default=5000)
    parser.add_argument("--hot-count", type=int, default=8)
    parser.add_argument("--member-delay", type=float, default=0.15)
    parser.add_argument("--deb-delay", type=float, default=8)
    parser.add_argument("--periodic-only", action="store_true")
    args = parser.parse_args()
    if min(args.delay, args.client_interval, args.max_wait) <= 0:
        parser.error("delay, client-interval, and max-wait must be positive")
    if args.suites < 1 or args.catalog_size < 1 or args.hot_count < 1 or min(args.member_delay, args.deb_delay) < 0:
        parser.error("invalid burst dimensions")
    if args.burst:
        from burst import BurstLab
        lab = BurstLab(args)
    else:
        lab = Lab(args)
    try:
        lab.run()
    except Exception as error:
        lab.event("failed", error=str(error))
        raise
    finally:
        lab.close()


if __name__ == "__main__":
    main()

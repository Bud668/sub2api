#!/usr/bin/env python3
"""Socket-activated installer. Only a tag crosses the unprivileged boundary.

The pinned release key authorizes the entire reviewed per-release deployment
bundle, including its existing backup, accounting and pre-start recovery gates.
No shell parameters, file paths, download URLs or keys come from the caller.
"""
import fcntl
import json
import os
from pathlib import Path
import pwd
import re
import shutil
import socket
import struct
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.parse
import urllib.request

REPO = "Bud668/sub2api"
STATE = Path("/var/lib/sub2api-updater")
KEY = Path("/etc/sub2api/bud-release-public.pem")
MANIFEST = Path("/etc/sub2api/managed-release.json")
REQUEST_LOCK = Path("/run/lock/sub2api-update-request.lock")
TAG = re.compile(r"(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})-(Bud|cyberaudit)\.([1-9][0-9]{0,5})")
TERMINAL = {"completed", "failed", "recovered", "blocked"}


def version(value, legacy=False):
    match = TAG.fullmatch(value) if isinstance(value, str) else None
    if not match or (not legacy and match[4] != "Bud"):
        raise ValueError("invalid Bud version")
    return tuple(int(match[i]) for i in (1, 2, 3, 5))


def run(*args, **kwargs):
    return subprocess.run(args, check=True, timeout=600, **kwargs)


def write_json(path, data):
    # Root-owned state is readable by the app, never writable by it.
    fd, name = tempfile.mkstemp(dir=path.parent)
    try:
        with os.fdopen(fd, "w") as out:
            json.dump(data, out)
            out.flush()
            os.fsync(out.fileno())
            os.fchmod(out.fileno(), 0o640)
            os.fchown(out.fileno(), 0, pwd.getpwnam("sub2api").pw_gid)
        os.replace(name, path)
        directory = os.open(path.parent, os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def status(phase, target):
    write_json(STATE / "status.json", {"phase": phase, "version": target, "updated_at": int(time.time())})


def allowed_url(url):
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != "https" or parsed.username or parsed.password or parsed.port not in (None, 443):
        raise ValueError("invalid release URL")
    if parsed.hostname == "github.com" and parsed.path.startswith(f"/{REPO}/releases/download/"):
        return
    if parsed.hostname not in {"release-assets.githubusercontent.com", "objects.githubusercontent.com"}:
        raise ValueError("untrusted release host")


class ReleaseRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        allowed_url(newurl)
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def download(url, path, limit):
    allowed_url(url)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), ReleaseRedirect())
    deadline = time.monotonic() + 600
    with opener.open(url, timeout=30) as response, path.open("xb") as out:
        size = 0
        while chunk := response.read(1024 * 1024):
            size += len(chunk)
            if size > limit or time.monotonic() > deadline:
                raise ValueError("release download limit")
            out.write(chunk)


def extract(archive, destination):
    # Flat reviewed bundles only. No extractall, links, devices, duplicate names,
    # path traversal, unbounded decompression or writes outside a fresh root dir.
    destination.mkdir(mode=0o700)
    names, total = set(), 0
    with tarfile.open(archive, "r:gz") as bundle:
        for member in bundle:
            if not member.isfile() or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}", member.name) or member.name in names:
                raise ValueError("invalid bundle entry")
            names.add(member.name)
            total += member.size
            if member.size < 0 or member.size > 500 * 1024**2 or total > 1024**3 or len(names) > 128:
                raise ValueError("bundle size limit")
            with bundle.extractfile(member) as src, (destination / member.name).open("xb") as out:
                shutil.copyfileobj(src, out, 1024 * 1024)
    if not {"prepare-host.sh", "deploy-reviewed-primary.sh", "managed-release.json", "sub2api", "ARTIFACT-SHA256SUMS"} <= names:
        raise ValueError("incomplete update bundle")


def read_archive_phase(archive):
    try:
        return (archive / "phase").read_text().strip()
    except FileNotFoundError:
        return "prepared"


def mapped_phase(phase):
    return {"prepared": "preparing", "stopping_old": "restarting", "old_stopped": "restarting",
            "installing": "restarting", "starting_new": "restarting", "verifying": "checking",
            "committed": "checking", "done": "completed", "untouched": "failed",
            "restoring_old": "restarting", "recovered": "recovered", "blocked": "blocked"}.get(phase, "blocked")


def install(target, package):
    prefix = f"https://github.com/{REPO}/releases/download/v{target}/"
    filename = f"sub2api_{target}_linux_amd64.update.tar.gz"
    download(prefix + filename, package / filename, 500 * 1024**2)
    download(prefix + filename + ".sig", package / "release.sig", 64)
    status("verifying", target)
    run("openssl", "pkeyutl", "-verify", "-pubin", "-inkey", str(KEY), "-rawin",
        "-in", str(package / filename), "-sigfile", str(package / "release.sig"))
    extracted = package / "verified"
    extract(package / filename, extracted)
    release = json.loads((extracted / "managed-release.json").read_text())
    if release.get("version") != target or release.get("status") != "accepted" or release.get("source_repo") != f"https://github.com/{REPO}":
        raise ValueError("release identity mismatch")
    status("preparing", target)
    try:
        prepared = run("/bin/bash", str(extracted / "prepare-host.sh"), "primary", capture_output=True, text=True)
    except subprocess.CalledProcessError as exc:
        (package / "prepare.log").write_text((exc.stdout or "") + (exc.stderr or ""))
        raise
    # prepare-host writes no secrets, but keep its details in the root-only archive.
    (package / "prepare.log").write_text(prepared.stdout)
    matches = re.findall(r"^archive_dir=(/var/backups/sub2api/release-[A-Za-z0-9.-]+-[A-Za-z0-9]{8})$", prepared.stdout, re.M)
    if len(matches) != 1:
        raise ValueError("backup archive not confirmed")
    archive = Path(matches[0])
    if archive.is_symlink() or archive.stat().st_uid != 0 or archive.stat().st_mode & 0o777 != 0o700:
        raise ValueError("unsafe backup archive")
    write_json(STATE / "active.json", {"version": target, "archive": str(archive)})
    shutil.copytree(extracted, archive / "stage", dirs_exist_ok=True)
    run("/bin/bash", str(archive / "stage/deploy-reviewed-primary.sh"), "launch", str(archive))
    # This is already an independent systemd job with its own recovery gate.
    # Browser/app disconnects cannot cancel preparation or cutover.
    last = None
    for _ in range(600):
        phase = mapped_phase(read_archive_phase(archive))
        if phase != last:
            status(phase, target)
            last = phase
        if phase in TERMINAL:
            return
        time.sleep(1)
    status("blocked", target)


def reconcile():
    if not (STATE / "status.json").exists():
        return
    current = json.loads((STATE / "status.json").read_text())
    if current["phase"] in TERMINAL or current["phase"] == "idle":
        return
    active = json.loads((STATE / "active.json").read_text()) if (STATE / "active.json").exists() else {}
    if active.get("version") == current.get("version") and active.get("archive"):
        phase = mapped_phase(read_archive_phase(Path(active["archive"])))
        # Never declare success solely because HTTP health happens to respond.
        status(phase if phase in TERMINAL else "blocked", current["version"])
    else:
        status("failed", current["version"])


def main():
    if sys.argv[1:] == ["reconcile"]:
        # A rejected parallel socket must not overwrite the live worker's status.
        with REQUEST_LOCK.open("a") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                return
            reconcile()
        return
    if sys.argv[1:]:
        raise ValueError("no command arguments allowed")
    connection = socket.socket(fileno=0)
    connection.settimeout(3)
    _, uid, _ = struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
    if uid not in (0, pwd.getpwnam("sub2api").pw_uid):
        return
    with connection.makefile("rb") as incoming:
        request = json.loads(incoming.readline(256))
    if not isinstance(request, dict) or set(request) != {"version"}:
        raise ValueError("only a version is accepted")
    target = request["version"]
    version(target)
    with REQUEST_LOCK.open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            connection.sendall(b'{"accepted":false}\n')
            return
        current = json.loads(MANIFEST.read_text())
        if current.get("status") != "accepted" or version(target) <= version(current["version"], legacy=True):
            connection.sendall(b'{"accepted":false}\n')
            return
        if (STATE / "active.json").exists():
            active = json.loads((STATE / "active.json").read_text())
            if active.get("archive") and mapped_phase(read_archive_phase(Path(active["archive"]))) not in TERMINAL:
                connection.sendall(b'{"accepted":false}\n')
                return
        package = Path(tempfile.mkdtemp(prefix="release-", dir=STATE))
        write_json(STATE / "active.json", {"version": target})
        status("downloading", target)
        try:
            connection.sendall(b'{"accepted":true}\n')
        except (BrokenPipeError, ConnectionResetError):
            pass  # Accepted job persists even when its browser has disconnected.
        connection.close()
        try:
            install(target, package)
        except Exception as exc:
            print("bud_update_failed=" + type(exc).__name__, flush=True)
            reconcile()


if __name__ == "__main__":
    main()

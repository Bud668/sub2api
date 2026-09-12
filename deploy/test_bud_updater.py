"""Offline installer security checks. Never connects to production or restarts it."""
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import socket
import tarfile
import tempfile
import unittest
from unittest.mock import patch
from types import SimpleNamespace

spec = importlib.util.spec_from_file_location("updater", Path(__file__).with_name("bud-updater.py"))
updater = importlib.util.module_from_spec(spec)
spec.loader.exec_module(updater)


class UpdaterChecks(unittest.TestCase):
    def test_socket_accepts_only_new_version_and_survives_disconnect(self):
        for target, accepted, disconnect in [("0.2.4-Bud.14", True, False), ("0.2.4-Bud.14", True, True), ("0.2.4-Bud.12", False, False)]:
            with self.subTest(target=target, disconnect=disconnect), tempfile.TemporaryDirectory() as temp:
                state = Path(temp)
                manifest = state / "manifest.json"
                manifest.write_text(json.dumps({"version": "0.2.4-cyberaudit.13", "status": "accepted"}))
                client, server = socket.socketpair()
                client.sendall(json.dumps({"version": target}).encode() + b'\n')
                if disconnect:
                    client.close()
                try:
                    with patch.object(updater, "STATE", state), patch.object(updater, "MANIFEST", manifest), patch.object(updater, "REQUEST_LOCK", state / "lock"), patch.object(updater.sys, "argv", ["installer"]), patch.object(updater.socket, "socket", return_value=server), patch.object(updater.pwd, "getpwnam", return_value=SimpleNamespace(pw_uid=os.getuid())), patch.object(updater, "write_json", side_effect=lambda path, data: path.write_text(json.dumps(data))), patch.object(updater, "install") as install:
                        updater.main()
                        self.assertEqual(install.call_count, int(accepted))
                        if not disconnect:
                            self.assertEqual(json.loads(client.recv(1024))["accepted"], accepted)
                        if accepted:
                            self.assertEqual(json.loads((state / 'status.json').read_text())['phase'], 'downloading')
                finally:
                    client.close()
                    server.close()

    def test_versions_and_trusted_urls(self):
        self.assertLess(updater.version("0.2.4-cyberaudit.13", legacy=True), updater.version("0.2.4-Bud.14"))
        self.assertLess(updater.version("0.2.4-Bud.99"), updater.version("0.2.5-Bud.1"))
        for invalid in ["0.2.4", "0.2.4-Bud.0", "0.2.4-Bud.014", "0.2.4-cyberaudit.14", "0.2.4-Bud.14;id", "../Bud.14", None]:
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                updater.version(invalid)
        for invalid in ["http://github.com/Bud668/sub2api/releases/download/x", "https://github.com/Wei-Shaw/sub2api/releases/download/x", "https://github.com.evil.test/Bud668/sub2api/releases/download/x", "file:///etc/passwd", "https://user@github.com/Bud668/sub2api/releases/download/x", "https://github.com:8080/Bud668/sub2api/releases/download/x"]:
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                updater.allowed_url(invalid)
        updater.allowed_url("https://github.com/Bud668/sub2api/releases/download/v0.2.4-Bud.14/x")
        updater.allowed_url("https://release-assets.githubusercontent.com/github-production-release-asset/x")

    def test_extraction_rejects_unsafe_entries(self):
        for name, kind in [("../sub2api", tarfile.REGTYPE), ("/sub2api", tarfile.REGTYPE), ("sub2api", tarfile.SYMTYPE), ("sub2api", tarfile.LNKTYPE), ("folder/sub2api", tarfile.REGTYPE), ("sub2api", tarfile.CHRTYPE)]:
            with self.subTest(name=name, kind=kind), tempfile.TemporaryDirectory() as temp:
                archive = Path(temp) / "release.tar.gz"
                with tarfile.open(archive, "w:gz") as bundle:
                    member = tarfile.TarInfo(name)
                    member.type = kind
                    member.linkname = "/etc/passwd"
                    bundle.addfile(member, io.BytesIO())
                with self.assertRaises(ValueError):
                    updater.extract(archive, Path(temp) / "extracted")
        with tempfile.TemporaryDirectory() as temp:
            archive = Path(temp) / "release.tar.gz"
            with tarfile.open(archive, "w:gz") as bundle:
                for _ in range(2):
                    bundle.addfile(tarfile.TarInfo("sub2api"), io.BytesIO())
            with self.assertRaises(ValueError):
                updater.extract(archive, Path(temp) / "extracted")

    def test_signature_required_before_deployment(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            private, public, archive, signature = [directory / name for name in ("private.pem", "public.pem", "release.tar.gz", "release.sig")]
            subprocess.run(["openssl", "genpkey", "-algorithm", "ED25519", "-out", str(private)], check=True)
            subprocess.run(["openssl", "pkey", "-in", str(private), "-pubout", "-out", str(public)], check=True)
            with tarfile.open(archive, "w:gz") as bundle:
                for name in ["prepare-host.sh", "deploy-reviewed-primary.sh", "sub2api", "ARTIFACT-SHA256SUMS", "managed-release.json"]:
                    content = json.dumps({"version": "0.2.4-Bud.14", "status": "accepted", "source_repo": "https://github.com/Bud668/sub2api"}).encode() if name == "managed-release.json" else b"test"
                    entry = tarfile.TarInfo(name)
                    entry.size = len(content)
                    bundle.addfile(entry, io.BytesIO(content))
            subprocess.run(["openssl", "pkeyutl", "-sign", "-inkey", str(private), "-rawin", "-in", str(archive), "-out", str(signature)], check=True)
            real_run = updater.run

            def fake_download(url, path, limit):
                path.write_bytes(signature.read_bytes() if url.endswith(".sig") else archive.read_bytes())

            def verify_only(*args, **kwargs):
                if args[0] == "/bin/bash":
                    raise RuntimeError("signature passed before prepare")
                return real_run(*args, **kwargs)

            for corrupted in (False, True):
                if corrupted:
                    signature.write_bytes(b"x" * 64)
                package = directory / str(corrupted)
                package.mkdir()
                with patch.object(updater, "KEY", public), patch.object(updater, "download", fake_download), patch.object(updater, "status"), patch.object(updater, "run", side_effect=verify_only):
                    if corrupted:
                        with self.assertRaises(subprocess.CalledProcessError):
                            updater.install("0.2.4-Bud.14", package)
                        self.assertFalse((package / "verified").exists())
                    else:
                        with self.assertRaisesRegex(RuntimeError, "signature passed"):
                            updater.install("0.2.4-Bud.14", package)

    def test_reconcile_never_guesses_success_or_restores_database(self):
        for previous, mapped in [("done", "completed"), ("recovered", "recovered"), ("starting_new", "blocked"), ("installing", "blocked"), ("blocked", "blocked")]:
            with self.subTest(previous=previous), tempfile.TemporaryDirectory() as temp:
                state = Path(temp)
                (state / "phase").write_text(previous)
                (state / "status.json").write_text(json.dumps({"phase": "restarting", "version": "0.2.4-Bud.14"}))
                (state / "active.json").write_text(json.dumps({"version": "0.2.4-Bud.14", "archive": temp}))
                with patch.object(updater, "STATE", state), patch.object(updater, "status") as status, patch.object(updater, "run") as run:
                    updater.reconcile()
                    status.assert_called_once_with(mapped, "0.2.4-Bud.14")
                    run.assert_not_called()


if __name__ == "__main__":
    unittest.main()

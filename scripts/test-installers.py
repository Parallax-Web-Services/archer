"""Offline bootstrap integration tests; no release downloads or real home installs.

Run: python scripts/test-installers.py
Windows also exercises POSIX sh through Git for Windows with a Linux uname fixture.
"""
import argparse
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SCRIPTS = Path(__file__).resolve().parent
DEFAULT_VERSION = "v0.3.0-alpha.2"


def shell_path(path):
    """Paths passed inside MSYS sh use its drive mapping, not backslashes."""
    value = str(path)
    if os.name == "nt" and len(value) > 1 and value[1] == ":":
        return "/" + value[0].lower() + value[2:].replace("\\", "/")
    return value.replace("\\", "/")


class InstallerTests(unittest.TestCase):
    kind = None

    def setUp(self):
        if not self.kind:
            self.skipTest("abstract fixture")
        self.temp = tempfile.TemporaryDirectory(prefix="archer-installer-test-")
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.root = self.base / "user home" / ".archer"
        self.dist = self.base / "dist"
        self.dist.mkdir()
        self.env = dict(os.environ)
        self.env.pop("ARCHER_VERSION", None)
        self.env["ARCHER_HOME"] = str(self.root)
        self.env["ARCHER_DIST_DIR"] = str(self.dist)
        if self.kind == "posix":
            self.shell = shutil.which("sh")
            if os.name == "nt":
                git_sh = Path(r"C:\Program Files\Git\usr\bin\sh.exe")
                self.shell = str(git_sh) if git_sh.is_file() else None
            if not self.shell:
                self.skipTest("POSIX sh unavailable")
            mocks = self.base / "mocks"
            mocks.mkdir()
            uname = mocks / "uname"
            uname.write_text("#!/bin/sh\ncase \"$1\" in -s) echo Linux;; -m) echo x86_64;; esac\n", encoding="ascii")
            uname.chmod(0o755)
            self.env["PATH"] = shell_path(mocks) + ":" + ("/usr/bin:/bin" if os.name == "nt" else os.environ.get("PATH", ""))
            self.env["ARCHER_HOME"] = shell_path(self.root)
            self.env["ARCHER_DIST_DIR"] = shell_path(self.dist)
            self.env["TMPDIR"] = shell_path(self.base)
            if os.name == "nt":
                self.env["MSYS"] = "winsymlinks:nativestrict"
            self.artifact = "archer_linux_amd64"
            self.binary = "archer"
            self.command = [self.shell, shell_path(SCRIPTS / "install.sh")]
        else:
            if os.name != "nt":
                self.skipTest("Windows installer requires Windows")
            ps = shutil.which("powershell") or shutil.which("pwsh")
            if not ps:
                self.skipTest("PowerShell unavailable")
            machine = os.environ.get("PROCESSOR_ARCHITEW6432", os.environ.get("PROCESSOR_ARCHITECTURE", "AMD64")).lower()
            arch = "arm64" if machine == "arm64" else "amd64"
            self.artifact = f"archer_windows_{arch}.exe"
            self.binary = "archer.exe"
            self.env["TEMP"] = self.env["TMP"] = str(self.base)
            self.command = [ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", str(SCRIPTS / "install.ps1")]
        self.fixture(b"fixture version one; bootstrap must not execute these bytes\n")

    def fixture(self, data, wrong=False):
        (self.dist / self.artifact).write_bytes(data)
        digest = "0" * 64 if wrong else hashlib.sha256(data).hexdigest()
        (self.dist / "SHA256SUMS").write_text(f"{digest}  {self.artifact}\n", encoding="ascii")
        self.data = data

    def install(self, version=None, ok=True):
        env = dict(self.env)
        if version is not None:
            env["ARCHER_VERSION"] = version
        # Copy scripts out of UNC workspaces: MSYS uses local paths consistently.
        command = list(self.command)
        if self.kind == "posix" and os.name == "nt":
            script = self.base / "install.sh"
            shutil.copyfile(SCRIPTS / "install.sh", script)
            command[-1] = shell_path(script)
        run = subprocess.run(command, env=env, capture_output=True, text=True, timeout=30, cwd=self.base)
        if ok:
            self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
            self.assertIn("PATH", run.stdout)
        else:
            self.assertNotEqual(run.returncode, 0, run.stdout + run.stderr)
        self.assertEqual(list(self.base.glob("archer-download-*")), [])
        self.assertEqual(list(self.base.glob("archer-download.*")), [])
        return run

    def version_binary(self, version=DEFAULT_VERSION):
        return self.root / "versions" / version / "bin" / self.binary

    def test_verified_install_and_repeat(self):
        self.install()
        self.assertEqual(self.version_binary().read_bytes(), self.data)
        self.install()
        self.assertEqual(self.version_binary().read_bytes(), self.data)
        self.assertEqual(sorted(p.name for p in (self.root / "versions").iterdir()), [DEFAULT_VERSION])

    def test_wrong_checksum_leaves_no_installation(self):
        self.fixture(b"corrupt fixture", wrong=True)
        run = self.install(ok=False)
        self.assertIn("checksum mismatch", (run.stdout + run.stderr).lower())
        self.assertFalse(self.root.exists())

    def test_switch_to_different_managed_version(self):
        self.install()
        old = self.version_binary().read_bytes()
        self.fixture(b"fixture version two\n")
        self.install("v0.2.0")
        self.assertEqual(self.version_binary().read_bytes(), old)
        self.assertEqual(self.version_binary("v0.2.0").read_bytes(), self.data)
        if self.kind == "posix":
            launcher = self.root / "bin" / "archer"
            # Git on Windows may emulate links as files unless symlink support is enabled.
            if launcher.is_symlink():
                self.assertIn("v0.2.0", os.readlink(launcher))
            else:
                self.assertIn(b"v0.2.0", launcher.read_bytes())
        else:
            self.assertIn("v0.2.0", (self.root / "bin" / "archer.cmd").read_text())

    def test_same_tag_changed_bytes_refused(self):
        self.install()
        old = self.version_binary().read_bytes()
        self.fixture(b"changed contents for immutable tag\n")
        run = self.install(ok=False)
        self.assertIn("differs from the pinned release", run.stdout + run.stderr)
        self.assertEqual(self.version_binary().read_bytes(), old)

    def test_unmanaged_binary_preserved(self):
        bin_dir = self.root / "bin"
        bin_dir.mkdir(parents=True)
        unmanaged = bin_dir / ("archer" if self.kind == "posix" else "archer.cmd")
        unmanaged.write_bytes(b"user-owned binary")
        self.install(ok=False)
        self.assertEqual(unmanaged.read_bytes(), b"user-owned binary")
        self.assertFalse((self.root / "versions").exists())

    def test_unsafe_version_refused(self):
        self.install("../../escape", ok=False)
        self.assertFalse(self.root.exists())

    def test_unmanaged_version_directory_preserved(self):
        version_dir = self.root / "versions" / DEFAULT_VERSION
        version_dir.mkdir(parents=True)
        (version_dir / "mine").write_text("keep")
        self.install(ok=False)
        self.assertEqual((version_dir / "mine").read_text(), "keep")

    def test_duplicate_checksum_refused(self):
        manifest = self.dist / "SHA256SUMS"
        manifest.write_text(manifest.read_text() * 2)
        self.install(ok=False)
        self.assertFalse(self.root.exists())

    def test_symlink_directory_refused(self):
        outside = self.base / "outside"
        outside.mkdir()
        self.root.parent.mkdir(parents=True)
        try:
            self.root.symlink_to(outside, target_is_directory=True)
        except (OSError, NotImplementedError):
            self.skipTest("directory symlink permission unavailable")
        self.install(ok=False)
        self.assertEqual(list(outside.iterdir()), [])


class PosixInstallerTests(InstallerTests):
    kind = "posix"


class WindowsInstallerTests(InstallerTests):
    kind = "windows"


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--platform", choices=("all", "posix", "windows"), default="all")
    choice = parser.parse_args().platform
    suite = unittest.TestSuite()
    for cls in (PosixInstallerTests, WindowsInstallerTests):
        if choice in ("all", cls.kind):
            suite.addTests(unittest.defaultTestLoader.loadTestsFromTestCase(cls))
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    raise SystemExit(0 if result.wasSuccessful() else 1)

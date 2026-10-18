"""Filesystem/security installer tests. Fixture binaries are not app validation."""
import fcntl
import hashlib
import io
import os
from pathlib import Path
import platform
import pwd
import subprocess
import tarfile
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "install.sh"
ARCH = "amd64" if platform.machine() == "x86_64" else "arm64"
MARKER = "PowerLinux rewrite installation v1\n"


class InstallTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="powerlinux-installer-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.prefix = self.root / "installed"

    def package(self, version="0.1.0-test", extra=None, change=None, include_folio=False):
        path = self.root / f"{version}.tar.gz"
        if path.exists() and extra is None and change is None and not include_folio:
            return path, hashlib.sha256(path.read_bytes()).hexdigest()
        members = {
            "powerlinux": f"#!/bin/bash\nif [[ $1 == version ]]; then echo '{version}'; else exit 31; fi\n".encode(),
            "VERSION": f"{version}\n".encode(),
            "PLATFORM": f"linux-{ARCH}\n".encode(),
            "install.sh": b"# installer fixture\n",
            "BUILDINFO": b"fixture=true\n",
            "README.txt": b"Fixture package; not a production application.\n",
        }
        if include_folio:
            members["folio"] = members["powerlinux"]
        if change:
            members.update(change)
        with tarfile.open(path, "w:gz", format=tarfile.USTAR_FORMAT) as archive:
            for name, value in members.items():
                info = tarfile.TarInfo(name)
                info.mode = 0o755 if name in ("folio", "powerlinux") else 0o644
                info.size = len(value)
                archive.addfile(info, io.BytesIO(value))
            if extra:
                for info, content in extra:
                    archive.addfile(info, io.BytesIO(content) if content is not None else None)
        return path, hashlib.sha256(path.read_bytes()).hexdigest()

    def run_installer(self, *args, success=True, env=None):
        result = subprocess.run(
            ["bash", str(SCRIPT), "--prefix", str(self.prefix), *map(str, args)],
            text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            env=env, timeout=15,
        )
        if success:
            self.assertEqual(result.returncode, 0, result.stdout)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout)
        return result

    def install(self, version="0.1.0-test", **kwargs):
        path, sha = self.package(version)
        return self.run_installer("--archive", path, "--sha256", sha, **kwargs)

    def test_valid_package_is_installed_without_system_service(self):
        self.install()
        self.assertEqual(os.readlink(self.prefix / "current"), "releases/0.1.0-test")
        self.assertTrue((self.prefix / "data").is_dir())
        self.assertTrue((self.prefix / "config").is_dir())
        self.assertFalse((self.prefix / "powerlinux.service").exists())
        binary = self.prefix / "current" / "powerlinux"
        self.assertEqual(subprocess.check_output([binary, "version"], text=True).strip(), "0.1.0-test")
        self.assertEqual((self.prefix / "current/folio").read_bytes(), binary.read_bytes())

    def test_new_package_with_folio_and_compatibility_commands_installs_both(self):
        path, sha = self.package(include_folio=True)
        self.run_installer("--archive", path, "--sha256", sha)
        folio = self.prefix / "current/folio"
        compatibility = self.prefix / "current/powerlinux"
        self.assertEqual(folio.read_bytes(), compatibility.read_bytes())
        self.assertEqual(subprocess.check_output([folio, "version"], text=True).strip(), "0.1.0-test")

    def test_idempotent_install_preserves_data_and_snapshot_count(self):
        self.install()
        note = self.prefix / "data" / "notes.txt"
        note.write_text("human and AI shared note")
        snapshots = list((self.prefix / "backups").iterdir())
        result = self.install()
        self.assertIn("already installed", result.stdout)
        self.assertEqual(note.read_text(), "human and AI shared note")
        self.assertEqual(list((self.prefix / "backups").iterdir()), snapshots)

    def test_wrong_hash_fails_before_touching_prefix(self):
        path, _ = self.package()
        result = self.run_installer("--archive", path, "--sha256", "0" * 64, success=False)
        self.assertIn("SHA256", result.stdout)
        self.assertFalse(self.prefix.exists())

    def test_missing_hash_is_rejected(self):
        path, _ = self.package()
        result = self.run_installer("--archive", path, success=False)
        self.assertIn("trusted SHA256", result.stdout)
        self.assertFalse(self.prefix.exists())

    def test_privileged_agent_cannot_be_enabled_in_prefix_mode(self):
        path, sha = self.package()
        policy = self.root / "policy.json"
        policy.write_text('{"version":2}')
        result = self.run_installer("--archive", path, "--sha256", sha, "--enable-agent", "--agent-policy", policy, success=False)
        self.assertIn("production", result.stdout)
        self.assertFalse(self.prefix.exists())

    def test_module_flags_require_explicit_agent_policy(self):
        path, sha = self.package()
        for option in ("--enable-terminal", "--enable-software", "--enable-sites", "--enable-source-build"):
            result = self.run_installer("--archive", path, "--sha256", sha, option, success=False)
            self.assertIn("requires --enable-agent", result.stdout)
        self.assertFalse(self.prefix.exists())

    def test_source_build_requires_software_before_any_filesystem_change(self):
        path, sha = self.package()
        policy = self.root / "policy.json"
        policy.write_text('{"version":2}')
        result = self.run_installer("--archive", path, "--sha256", sha,
                                    "--enable-agent", "--agent-policy", policy,
                                    "--enable-source-build", success=False)
        self.assertIn("requires --enable-software", result.stdout)
        self.assertFalse(self.prefix.exists())

    def test_module_preflight_rejects_real_accounts_sharing_api_primary_group(self):
        # Read-only account preflight on Debian/Ubuntu's actual existing
        # identities. Only the hard-coded API account is selected for this
        # fixture; there is no account creation or home/permission mutation.
        try:
            api = pwd.getpwnam("_apt")
            module = pwd.getpwnam("nobody")
        except KeyError:
            self.skipTest("distro shared-primary-group fixture is absent")
        if api.pw_uid == module.pw_uid or api.pw_gid != module.pw_gid:
            self.skipTest("distro accounts do not share the primary-group fixture")
        source = SCRIPT.read_text()
        start = source.index("module_user() {")
        end = source.index('  [[ ! -L "$home_dir" ]]', start)
        preflight = source[start:end] + "}\n"
        # A disappearing account fails instead of ever invoking useradd.
        creation = '    useradd --system --user-group --home-dir "$home_dir" --no-create-home --shell /usr/sbin/nologin "$account"'
        self.assertIn(creation, preflight)
        preflight = preflight.replace(creation, "    die 'fixture account disappeared'")
        preflight = preflight.replace("id -u powerlinux", 'id -u "$api_fixture"').replace("id -g powerlinux", 'id -g "$api_fixture"')
        code = "set -Eeuo pipefail\ndie() { printf '%s\\n' \"$*\" >&2; exit 1; }\napi_fixture=$1\n" + preflight + '\nmodule_user "$2" "$3"\n'
        result = subprocess.run(["bash", "-c", code, "account-preflight", api.pw_name, module.pw_name, module.pw_dir],
                                text=True, capture_output=True, timeout=5)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("UID/GID distinct from the API", result.stderr)

    def test_path_traversal_archive_rejected(self):
        info = tarfile.TarInfo("../escape")
        info.size = 4
        path, sha = self.package(extra=[(info, b"oops")])
        self.run_installer("--archive", path, "--sha256", sha, success=False)
        self.assertFalse(self.prefix.exists())
        self.assertFalse((self.root / "escape").exists())

    def test_duplicate_member_rejected(self):
        info = tarfile.TarInfo("VERSION")
        info.size = 4
        path, sha = self.package(extra=[(info, b"oops")])
        result = self.run_installer("--archive", path, "--sha256", sha, success=False)
        self.assertIn("Duplicate", result.stdout)

    def test_symlink_member_rejected(self):
        path, _ = self.package()
        with tarfile.open(path, "r:gz") as original:
            members = [(i, original.extractfile(i).read()) for i in original]
        with tarfile.open(path, "w:gz", format=tarfile.USTAR_FORMAT) as archive:
            for info, content in members:
                if info.name == "powerlinux":
                    info.type = tarfile.SYMTYPE
                    info.linkname = "/bin/bash"
                    info.size = 0
                    archive.addfile(info)
                else:
                    archive.addfile(info, io.BytesIO(content))
        sha = hashlib.sha256(path.read_bytes()).hexdigest()
        result = self.run_installer("--archive", path, "--sha256", sha, success=False)
        self.assertIn("non-regular", result.stdout)
        self.assertFalse(self.prefix.exists())

    def test_wrong_platform_rejected(self):
        wrong = "arm64" if ARCH == "amd64" else "amd64"
        path, sha = self.package(change={"PLATFORM": f"linux-{wrong}\n".encode()})
        result = self.run_installer("--archive", path, "--sha256", sha, success=False)
        self.assertIn("platform", result.stdout)
        self.assertFalse(self.prefix.exists())

    def test_binary_version_mismatch_rejected(self):
        path, sha = self.package(change={"VERSION": b"0.9.0-test\n"})
        result = self.run_installer("--archive", path, "--sha256", sha, success=False)
        self.assertIn("Binary and package", result.stdout)
        self.assertFalse(self.prefix.exists())

    def test_unmanaged_directory_preserved(self):
        self.prefix.mkdir()
        existing = self.prefix / "user-file"
        existing.write_text("keep")
        self.install(success=False)
        self.assertEqual(existing.read_text(), "keep")
        self.assertFalse((self.prefix / ".powerlinux-install").exists())

    def test_prefix_symlink_rejected(self):
        target = self.root / "target"
        target.mkdir()
        self.prefix.symlink_to(target, target_is_directory=True)
        self.install(success=False)
        self.assertEqual(list(target.iterdir()), [])

    def test_existing_version_cannot_be_replaced(self):
        self.install()
        path, sha = self.package(change={"README.txt": b"changed artifact"})
        result = self.run_installer("--archive", path, "--sha256", sha, success=False)
        self.assertIn("different artifact", result.stdout)
        self.assertEqual(os.readlink(self.prefix / "current"), "releases/0.1.0-test")

    def test_installed_binary_tampering_is_detected(self):
        path, sha = self.package()
        self.run_installer("--archive", path, "--sha256", sha)
        (self.prefix / "current" / "powerlinux").write_text("tampered")
        result = self.run_installer("--archive", path, "--sha256", sha, success=False)
        self.assertIn("modified", result.stdout)

    def test_installed_folio_command_tampering_is_detected(self):
        path, sha = self.package(include_folio=True)
        self.run_installer("--archive", path, "--sha256", sha)
        (self.prefix / "current/folio").write_text("tampered")
        result = self.run_installer("--archive", path, "--sha256", sha, success=False)
        self.assertIn("Folio command was modified", result.stdout)

    def test_upgrade_rollback_restores_previous_snapshot_and_preserves_new_data(self):
        self.install()
        original_lock_inode = (self.prefix / "data/server.lock").stat().st_ino
        note = self.prefix / "data" / "notes.txt"
        note.write_text("before upgrade")
        self.install("0.2.0-test")
        self.assertEqual(os.readlink(self.prefix / "current"), "releases/0.2.0-test")
        note.write_text("after upgrade")
        self.run_installer("--rollback")
        self.assertEqual(os.readlink(self.prefix / "current"), "releases/0.1.0-test")
        self.assertEqual(note.read_text(), "before upgrade")
        self.assertEqual((self.prefix / "data/server.lock").stat().st_ino, original_lock_inode)
        copies = list((self.prefix / "backups").glob("*/data/notes.txt"))
        self.assertTrue(any(p.read_text() == "after upgrade" for p in copies))

    def test_uninstall_preserves_data_and_is_repeatable(self):
        self.install()
        note = self.prefix / "data" / "notes.txt"
        note.write_text("retained")
        self.run_installer("--uninstall")
        self.run_installer("--uninstall")
        self.assertFalse((self.prefix / "current").exists())
        self.assertEqual(note.read_text(), "retained")
        self.assertTrue((self.prefix / "releases/0.1.0-test/powerlinux").exists())

    def test_install_lock_excludes_concurrent_installer(self):
        self.install()
        with (self.prefix / ".install-lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = self.install(success=False)
        self.assertIn("Another installer", result.stdout)

    def test_server_lock_prevents_live_data_update(self):
        self.install()
        with (self.prefix / "data/server.lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = self.install("0.2.0-test", success=False)
        self.assertIn("Stop the prefix-mode server", result.stdout)
        self.assertEqual(os.readlink(self.prefix / "current"), "releases/0.1.0-test")

    def test_failed_atomic_switch_restores_prior_release_and_data(self):
        self.install()
        note = self.prefix / "data" / "notes.txt"
        note.write_text("keep after failure")
        wrappers = self.root / "wrappers"
        wrappers.mkdir()
        sentinel = wrappers / "failed-once"
        wrapper = wrappers / "mv"
        wrapper.write_text(
            "#!/bin/bash\n"
            f"if [[ ${{!#}} == '{self.prefix}/current' && ! -e '{sentinel}' ]]; then touch '{sentinel}'; exit 73; fi\n"
            'exec /usr/bin/mv "$@"\n'
        )
        wrapper.chmod(0o755)
        env = dict(os.environ, PATH=f"{wrappers}:{os.environ['PATH']}")
        result = self.install("0.2.0-test", success=False, env=env)
        self.assertIn("restoring the prior release", result.stdout)
        self.assertEqual(os.readlink(self.prefix / "current"), "releases/0.1.0-test")
        self.assertEqual(note.read_text(), "keep after failure")
        self.assertFalse((self.prefix / "pending-transaction").exists())

    def test_interrupted_transaction_recovers_before_new_operation(self):
        self.install()
        note = self.prefix / "data" / "notes.txt"
        note.write_text("pre-upgrade")
        self.install("0.2.0-test")
        old_snapshot = (self.prefix / "rollback-target").read_text()
        note.write_text("incomplete-upgrade")
        (self.prefix / "pending-transaction").write_text(old_snapshot)
        result = self.run_installer("--uninstall", success=False)
        self.assertIn("Interrupted installation detected", result.stdout)
        self.assertEqual(note.read_text(), "pre-upgrade")
        self.assertEqual(os.readlink(self.prefix / "current"), "releases/0.1.0-test")
        self.assertFalse((self.prefix / "pending-transaction").exists())
        failed = list((self.prefix / "backups").glob("*/failed-data/notes.txt"))
        self.assertTrue(any(p.read_text() == "incomplete-upgrade" for p in failed))

    def test_interrupted_transaction_does_not_replace_live_server_data(self):
        self.install()
        self.install("0.2.0-test")
        pending = (self.prefix / "rollback-target").read_text()
        (self.prefix / "pending-transaction").write_text(pending)
        note = self.prefix / "data/notes.txt"
        note.write_text("live data")
        with (self.prefix / "data/server.lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.run_installer("--uninstall", success=False)
        self.assertEqual(note.read_text(), "live data")
        self.assertEqual((self.prefix / "pending-transaction").read_text(), pending)
        self.assertEqual(os.readlink(self.prefix / "current"), "releases/0.2.0-test")

    def test_failed_data_preservation_leaves_current_data_and_recovery_journal(self):
        self.install()
        note = self.prefix / "data/notes.txt"
        note.write_text("must not disappear")
        wrappers = self.root / "wrappers"
        wrappers.mkdir()
        wrapper = wrappers / "mv"
        wrapper.write_text(
            "#!/bin/bash\n"
            f"if [[ ${{!#}} == '{self.prefix}/current' ]]; then exit 73; fi\n"
            'exec /usr/bin/mv "$@"\n'
        )
        wrapper.chmod(0o755)
        copy_wrapper = wrappers / "cp"
        copy_wrapper.write_text(
            "#!/bin/bash\n"
            'if [[ ${!#} == */failed-data/ ]]; then exit 71; fi\n'
            'exec /usr/bin/cp "$@"\n'
        )
        copy_wrapper.chmod(0o755)
        env = dict(os.environ, PATH=f"{wrappers}:{os.environ['PATH']}")
        result = self.install("0.2.0-test", success=False, env=env)
        self.assertIn("Failed to preserve current data", result.stdout)
        self.assertEqual(note.read_text(), "must not disappear")
        self.assertTrue((self.prefix / "pending-transaction").exists())
        # Retrying without the simulated full-disk copy failure performs recovery.
        self.run_installer("--uninstall", success=False)
        self.assertEqual(note.read_text(), "must not disappear")
        self.assertFalse((self.prefix / "pending-transaction").exists())


if __name__ == "__main__":
    unittest.main()

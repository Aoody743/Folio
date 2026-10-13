"""Real kernel flock/inode checks; production ownership/systemd need QA VMs."""
import fcntl
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "install.sh"


class ProductionDataLockTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="powerlinux-data-lock-")
        self.addCleanup(temporary.cleanup)
        self.data = Path(temporary.name)
        source = SCRIPT.read_text()
        self.functions = source[source.index("acquire_production_data_lock() {"):
                                source.index("cleanup() {")]

    def script(self, after=""):
        # Only the owner-changing call is replaced in this ordinary-UID unit
        # fixture. Its exact root-service owner/FD arguments remain checked.
        # Kernel file locks, open/stat/chmod, and content changes are real.
        return """set -Eeuo pipefail
umask 077
production=1
production_data_locked=0
data=$1
die() { printf '%s\\n' "$*" >&2; exit 1; }
chown() { [[ $1 == powerlinux:powerlinux && $2 == /proc/$$/fd/8 ]]; }
""" + self.functions + "\nacquire_production_data_lock\n" + after

    def run_lock(self, after="", success=True):
        result = subprocess.run(["bash", "-c", self.script(after), "fixture", str(self.data)],
                                text=True, capture_output=True, timeout=5)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    def test_same_inode_excludes_another_process_then_releases(self):
        lock = self.data / "server.lock"
        lock.write_text("preserved lock contents")
        original_inode = lock.stat().st_ino
        worker = subprocess.Popen(["bash", "-c", self.script(
            "printf 'locked\\n'; read -r -t 3 release; release_production_data_lock"),
            "fixture", str(self.data)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True)
        self.addCleanup(lambda: worker.poll() is None and worker.kill())
        self.assertEqual(worker.stdout.readline(), "locked\n")
        with lock.open("r") as contender:
            with self.assertRaises(BlockingIOError):
                fcntl.flock(contender, fcntl.LOCK_EX | fcntl.LOCK_NB)
            output, error = worker.communicate("release\n", timeout=5)
            self.assertEqual(worker.returncode, 0, output + error)
            fcntl.flock(contender, fcntl.LOCK_EX | fcntl.LOCK_NB)
        self.assertEqual(lock.stat().st_ino, original_inode)
        self.assertEqual(lock.stat().st_mode & 0o777, 0o600)
        self.assertEqual(lock.read_text(), "preserved lock contents")

    def test_held_maintenance_lock_is_rejected_without_mutation(self):
        lock = self.data / "server.lock"
        lock.write_text("local CLI operation owns this inode")
        lock.chmod(0o640)
        with lock.open("r") as holder:
            fcntl.flock(holder, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = self.run_lock(success=False)
        self.assertIn("local maintenance command still holds", result.stderr)
        self.assertEqual(lock.read_text(), "local CLI operation owns this inode")
        self.assertEqual(lock.stat().st_mode & 0o777, 0o640)

    def test_creation_and_unsafe_names_are_bounded(self):
        lock = self.data / "server.lock"
        self.run_lock("release_production_data_lock")
        self.assertEqual(lock.read_bytes(), b"")
        self.assertEqual(lock.stat().st_mode & 0o777, 0o600)
        lock.unlink()
        victim = self.data / "victim"
        victim.write_text("untouched victim")
        lock.symlink_to(victim)
        self.run_lock(success=False)
        self.assertEqual(victim.read_text(), "untouched victim")
        lock.unlink()
        os.mkfifo(lock)
        result = self.run_lock(success=False)
        self.assertIn("regular file", result.stderr)

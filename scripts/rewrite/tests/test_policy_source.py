"""Read-only policy-source validation; root positives run only in the owned VM."""
from pathlib import Path
import os
import subprocess
import tempfile
import unittest


INSTALLER = Path(os.environ.get("POWERLINUX_INSTALLER_UNDER_TEST", str(Path(__file__).resolve().parents[1] / "install.sh")))


class PolicySourceTests(unittest.TestCase):
    def validate(self, policy):
        source = INSTALLER.read_text()
        start = source.index("require_agent_policy_source() {")
        end = source.index("\nconfigure_deployment() {", start)
        # Exercise the actual shell validator without running installation or
        # touching users, services, configuration or any unrelated host files.
        code = "set -eu\ndie() { printf '%s\\n' \"$*\" >&2; exit 1; }\nagent_policy=$1\n" + source[start:end] + "\nrequire_agent_policy_source\n"
        return subprocess.run(["bash", "-c", code, "policy-source-test", str(policy)], capture_output=True, text=True, timeout=5)

    def test_foreign_owner_rejected_without_modification(self):
        with tempfile.TemporaryDirectory(prefix="powerlinux-policy-source-") as value:
            path = Path(value) / "policy.json"
            path.write_text('{}\n')
            path.chmod(0o600)
            if os.geteuid() == 0:
                os.chown(path, 65534, -1)
            before = path.stat()
            result = self.validate(path)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("root-owned regular file", result.stderr)
            after = path.stat()
            self.assertEqual((before.st_ino, before.st_mode, before.st_uid, before.st_mtime_ns),
                             (after.st_ino, after.st_mode, after.st_uid, after.st_mtime_ns))
            self.assertEqual(path.read_text(), '{}\n')

    @unittest.skipUnless(os.geteuid() == 0, "root-owned positive policy-source cases require the owned QA VM")
    def test_root_private_policy_accepted_and_unsafe_files_rejected(self):
        with tempfile.TemporaryDirectory(prefix="powerlinux-policy-source-") as value:
            directory = Path(value)
            path = directory / "policy.json"
            path.write_text('{}\n')
            for mode in (0o600, 0o400):
                path.chmod(mode)
                result = self.validate(path)
                self.assertEqual(result.returncode, 0, result.stderr)
            path.chmod(0o640)
            self.assertNotEqual(self.validate(path).returncode, 0)
            path.chmod(0o600)
            link = directory / "link.json"
            link.symlink_to(path)
            self.assertNotEqual(self.validate(link).returncode, 0)
            fifo = directory / "fifo.json"
            os.mkfifo(fifo, 0o600)
            self.assertNotEqual(self.validate(fifo).returncode, 0)
            self.assertNotEqual(self.validate("policy.json").returncode, 0)

    @unittest.skipUnless(os.geteuid() == 0, "root-owned positive policy-source cases require the owned QA VM")
    def test_policy_name_is_protected_from_untrusted_parent_replacement(self):
        with tempfile.TemporaryDirectory(prefix="powerlinux-policy-source-") as value:
            parent = Path(value) / "parent"
            parent.mkdir(mode=0o700)
            policy = parent / "policy.json"
            policy.write_text('{}\n')
            policy.chmod(0o600)
            self.assertEqual(self.validate(policy).returncode, 0)
            os.chown(parent, 65534, -1)
            self.assertNotEqual(self.validate(policy).returncode, 0)
            os.chown(parent, 0, -1)
            parent.chmod(0o777)
            self.assertNotEqual(self.validate(policy).returncode, 0)
            parent.chmod(0o1777)
            result = self.validate(policy)
            self.assertEqual(result.returncode, 0, result.stderr)
            parent.chmod(0o700)
            alias = Path(value) / "alias"
            alias.symlink_to(parent, target_is_directory=True)
            self.assertNotEqual(self.validate(alias / "policy.json").returncode, 0)


if __name__ == "__main__":
    unittest.main()

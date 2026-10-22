"""Installer presentation must preserve transaction behavior and unattended use."""
import os
from pathlib import Path
import pty
import re
import select
import signal
import stat
import subprocess
import time
import unittest
import test_install as fixture

class InstallerExperienceTests(unittest.TestCase):
    def setUp(self):
        self.fixture = fixture.InstallTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)

    def test_each_language_installs_and_keeps_a_private_summary(self):
        for language, ready in [('en', 'The package is ready'), ('zh-CN', '安装包已就绪')]:
            with self.subTest(language=language):
                self.fixture.prefix = self.fixture.root / ('installed-' + language)
                result = self.fixture.run_installer(
                    '--archive', self.fixture.package()[0], '--sha256', self.fixture.package()[1], '--lang', language, '--yes')
                self.assertIn(ready, result.stdout)
                self.assertIn('[6/6]', result.stdout)
                match = re.search(r'(?:summary:|私有安装摘要：)\s*(\S+)', result.stdout)
                self.assertIsNotNone(match, result.stdout)
                path = Path(match[1])
                self.addCleanup(path.unlink, missing_ok=True)
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
                self.assertNotIn('\x1b', result.stdout)
                note = self.fixture.prefix / 'data' / 'kept.txt'
                note.write_text('keep this data')
                self.fixture.run_installer('--archive', self.fixture.package()[0], '--sha256', self.fixture.package()[1], '--lang', language, '--yes')
                self.assertEqual(note.read_text(), 'keep this data')
        self.assertEqual(note.read_text(), 'keep this data')

    def test_pipe_script_stream_never_waits_for_a_terminal_answer(self):
        archive, digest = self.fixture.package()
        result = subprocess.run(['bash', '-s', '--', '--archive', str(archive), '--sha256', digest,
                                 '--prefix', str(self.fixture.prefix), '--lang', 'en'],
                                input=fixture.SCRIPT.read_text(), text=True, capture_output=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('[6/6]', result.stdout)
        self.assertNotIn('Continue?', result.stdout)

    def test_interactive_decline_leaves_installation_untouched(self):
        archive, digest = self.fixture.package()
        pid, fd = pty.fork()
        if pid == 0:
            os.execvp('bash', ['bash', str(fixture.SCRIPT), '--archive', str(archive), '--sha256', digest,
                              '--prefix', str(self.fixture.prefix), '--lang', 'en'])
        output = b''
        try:
            deadline = time.monotonic() + 12
            while b'Continue? [Y/n]' not in output and time.monotonic() < deadline:
                if select.select([fd], [], [], .2)[0]:
                    try: output += os.read(fd, 65536)
                    except OSError: break
            self.assertIn(b'Continue? [Y/n]', output)
            self.assertFalse(self.fixture.prefix.exists())
            os.write(fd, b'n\n')
            while time.monotonic() < deadline:
                ended, status = os.waitpid(pid, os.WNOHANG)
                if ended:
                    self.assertEqual(os.waitstatus_to_exitcode(status), 0)
                    pid = None
                    break
                time.sleep(.05)
            self.assertIsNone(pid, 'installer did not stop after declining')
            self.assertFalse(self.fixture.prefix.exists())
        finally:
            if pid:
                os.kill(pid, signal.SIGKILL); os.waitpid(pid, 0)
            os.close(fd)

    def test_invalid_language_and_missing_value_fail_before_mutation(self):
        for args in [('--lang', 'fr'), ('--lang',)]:
            result = self.fixture.run_installer(*args, success=False)
            self.assertIn('--lang', result.stdout)
            self.assertFalse(self.fixture.prefix.exists())

    def test_chinese_failure_is_explained_without_printing_checksum_arguments(self):
        archive, digest = self.fixture.package()
        result = self.fixture.run_installer('--archive', archive, '--sha256', '0'*64,
                                            '--lang', 'zh-CN', success=False)
        self.assertIn('SHA256', result.stdout)
        self.assertIn('安装', result.stdout)
        self.assertNotIn('0'*64, result.stdout)
        self.assertFalse(self.fixture.prefix.exists())

    def test_bootstrap_help_supports_both_languages_without_downloading(self):
        bootstrap = fixture.SCRIPT.with_name('install-1.0-beta4.sh')
        for language, label in [('en', 'Install Folio'), ('zh-CN', '安装向导')]:
            result = subprocess.run(['bash', str(bootstrap), '--help', '--lang', language],
                                    capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn(label, result.stdout)

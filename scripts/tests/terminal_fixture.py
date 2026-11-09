"""Drive real terminal launches without a model service or user configuration."""
import errno
import os
import select
import signal
import subprocess
import time


class TerminalLaunch:
    def __init__(self, argv, *, cwd, env):
        import pty

        self.master, slave = pty.openpty()
        self.output = bytearray()
        try:
            self.process = subprocess.Popen(argv, cwd=cwd, env=env, stdin=slave, stdout=slave,
                                            stderr=slave, start_new_session=True)
        except BaseException:
            os.close(self.master)
            raise
        finally:
            os.close(slave)

    def read(self, timeout):
        if select.select([self.master], [], [], timeout)[0]:
            try:
                chunk = os.read(self.master, 65536)
            except OSError as error:
                if error.errno != errno.EIO:
                    raise
                return False
            self.output.extend(chunk)
            return bool(chunk)
        return True

    def until(self, text, timeout=10):
        deadline = time.monotonic() + timeout
        while text.encode() not in self.output:
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not self.read(remaining):
                raise AssertionError("Terminal did not display " + repr(text) + ": " + self.transcript())
        return self.transcript()

    def send(self, value):
        os.write(self.master, value.encode())

    def finish(self, timeout=10):
        deadline = time.monotonic() + timeout
        while self.process.poll() is None:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise AssertionError("Terminal did not exit: " + self.transcript())
            if not self.read(min(remaining, 0.1)):
                self.process.wait(timeout=remaining)
        while select.select([self.master], [], [], 0)[0] and self.read(0):
            pass
        return self.process.returncode

    def transcript(self):
        return self.output.decode("utf-8", errors="replace")

    def close(self):
        if self.process.poll() is None:
            os.killpg(self.process.pid, signal.SIGKILL)
            self.process.wait(timeout=5)
        os.close(self.master)


RECORD_LAUNCH = '''
from pathlib import Path
record={'argv':sys.argv[1:], 'environment':{name:os.environ[name] for name in
        ('OPENAI_API_KEY','CODEX_API_KEY','OPENAI_BASE_URL','HTTPS_PROXY','CPA_API_KEY') if name in os.environ}}
Path(os.environ['CWR_FIXTURE_RECORD']).write_text(json.dumps(record))
print('FIXTURE_CODEX_STARTED',flush=True)
if os.environ.get('CWR_FIXTURE_HOLD'):
    sys.stdin.readline()
sys.exit(7)
'''

#!/usr/bin/env python3
"""Opt-in Linux file-work sandbox. No model login, network or sudo fallback.

Only OS runtime files, this work's inputs/outputs and one restricted /tool
socket survive pivot_root. Host configuration is consumed before isolation.
"""

import argparse
import ctypes
import hashlib
import json
import os
from pathlib import Path
import platform
import signal
import stat
import subprocess
import sys
import tempfile


SOCKET_PATH = "/run/cc-connect/action-tools.sock"
OS_RUNTIME = ("/usr/bin", "/usr/lib", "/usr/lib64", "/bin", "/lib", "/lib64")
NAMESPACES = ("mnt", "pid", "user", "net", "ipc", "uts")
CAPABILITY_ENV = ("CC_FILE_ACTION_TOKEN", "MYANC_CRM_ACTION_TOKEN")
MS_RDONLY, MS_NOSUID, MS_NODEV = 1, 2, 4
MS_REMOUNT, MS_BIND, MS_REC, MS_PRIVATE = 32, 4096, 16384, 262144


class Refused(RuntimeError):
    pass


def absolute(value):
    if not isinstance(value, str) or not value.startswith("/"):
        raise Refused("absolute path required")
    path = Path(value)
    if str(path) != value or ".." in path.parts:
        raise Refused("canonical path required")
    return path


def pin(path, fds, directory=False):
    """Walk without symlinks; retain the object, not a mutable pathname."""
    path = absolute(str(path))
    fd = os.open("/", os.O_PATH | os.O_DIRECTORY)
    try:
        for part in path.parts[1:]:
            next_fd = os.open(part, os.O_PATH | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = next_fd
            if stat.S_ISLNK(os.fstat(fd).st_mode):
                raise Refused("symlink in protected path")
        if directory and not stat.S_ISDIR(os.fstat(fd).st_mode):
            raise Refused("directory required")
        fds.append(fd)
        return fd
    except BaseException:
        os.close(fd)
        raise


def protected(fd, owner):
    info = os.fstat(fd)
    if info.st_uid != owner or info.st_mode & 0o022:
        raise Refused("protected ownership and permissions required")


def check_tree(path):
    # Inputs are host-registered regular files. Existing outputs cannot smuggle
    # a host object through a hardlink, symlink, device or Unix socket.
    for root, dirs, files in os.walk(path, followlinks=False):
        for name in dirs + files:
            info = os.lstat(Path(root) / name)
            if stat.S_ISDIR(info.st_mode):
                continue
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
                raise Refused("work tree contains an unsupported object")


def prepare(config_path, work_root, command, fds):
    config_fd = pin(config_path, fds)
    info = os.fstat(config_fd)
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        raise Refused("regular configuration required")
    # The adapter also checks the launcher/config before opting into this
    # capability. A non-root model UID may never author its own allowlist.
    if info.st_uid == os.geteuid() and os.geteuid() != 0:
        raise Refused("configuration must belong to the host authority")
    protected(config_fd, info.st_uid)
    with open(f"/proc/self/fd/{config_fd}", encoding="utf-8") as stream:
        config = json.load(stream)
    if set(config) != {"enabled", "work_base_dir", "action_tools_socket", "command"}:
        raise Refused("unknown or missing configuration field")
    if config["enabled"] is not True:
        raise Refused("file-work sandbox disabled")
    base = absolute(config["work_base_dir"])
    work = absolute(work_root)
    if work.parent != base or not work.name:
        raise Refused("work must be a direct child of protected base")
    for reserved in (*OS_RUNTIME, "/proc", "/dev", "/run", "/home/model"):
        if work.is_relative_to(reserved) or Path(reserved).is_relative_to(work):
            raise Refused("work overlaps sandbox runtime")
    for path in (Path(config_path).parent, base, work, work / "inputs"):
        protected(pin(path, fds, directory=True), info.st_uid)
    output_fd = pin(work / "outputs", fds, directory=True)
    protected(output_fd, os.geteuid())
    check_tree(work / "inputs")
    check_tree(work / "outputs")
    approved = absolute(config["command"])
    if not command or command[0] != str(approved):
        raise Refused("command differs from protected allowlist")
    executable_fd = pin(approved, fds)
    executable_info = os.fstat(executable_fd)
    if not stat.S_ISREG(executable_info.st_mode) or not executable_info.st_mode & 0o111:
        raise Refused("executable required")
    protected(executable_fd, 0)
    socket_fd = pin(absolute(config["action_tools_socket"]), fds)
    if not stat.S_ISSOCK(os.fstat(socket_fd).st_mode):
        raise Refused("restricted action tools socket required")
    client_fd = pin(Path(__file__).resolve().with_name("file_tool.py"), fds)
    client_info = os.fstat(client_fd)
    if (not stat.S_ISREG(client_info.st_mode) or client_info.st_nlink != 1
            or client_info.st_uid not in {0, info.st_uid} or client_info.st_size > 128 * 1024):
        raise Refused("protected adjacent file client required")
    protected(client_fd, client_info.st_uid)
    with open(f"/proc/self/fd/{client_fd}", "rb") as stream:
        client = stream.read(128 * 1024 + 1)
    if len(client) > 128 * 1024:
        raise Refused("file client size exceeded")
    mounts = []
    for value in OS_RUNTIME:
        path = Path(value)
        if not path.exists():
            continue
        target = str(path.resolve(strict=True))
        runtime_fd = pin(target, fds, directory=True)
        protected(runtime_fd, 0)
        mounts.append([runtime_fd, target, True, True])
    mounts += [[pin(work / "inputs", fds, directory=True), str(work / "inputs"), True, True],
               [output_fd, str(work / "outputs"), True, False],
               [socket_fd, SOCKET_PATH, False, False]]
    if not any(approved.is_relative_to(Path(item[1])) for item in mounts[:len(mounts)-3]):
        mounts.append([executable_fd, str(approved), False, True])
    for name in ("null", "zero", "random", "urandom"):
        mounts.append([pin(Path("/dev") / name, fds), "/dev/" + name, False, False])
    return {"mounts": mounts, "work": str(work), "command": command,
            "client": client.decode("utf-8"), "client_sha256": hashlib.sha256(client).hexdigest(),
            "parent_ns": {name: os.readlink("/proc/self/ns/" + name) for name in NAMESPACES}}


def mount(source, target, filesystem=None, flags=0, data=None):
    libc = ctypes.CDLL(None, use_errno=True)
    args = [os.fsencode(value) if value is not None else None
            for value in (source, target, filesystem)]
    if libc.mount(*args, ctypes.c_ulong(flags), os.fsencode(data) if data else None) != 0:
        raise Refused("namespace mount unavailable")


def inside(state_fd):
    with os.fdopen(state_fd) as stream:
        state = json.load(stream)
    # Internal mode must never change the host mount tree, even if invoked by
    # hand. unshare --mount-proc supplies the child PID namespace view first.
    if os.getpid() != 1 or any(os.readlink("/proc/self/ns/" + name) == state["parent_ns"][name]
                             for name in NAMESPACES):
        raise Refused("fresh private namespaces required")
    if ctypes.CDLL(None, use_errno=True).sethostname(b"file-work", 9) != 0:
        raise Refused("private hostname unavailable")
    root = Path(state["root"])
    mount(None, "/", flags=MS_REC | MS_PRIVATE)
    mount("tmpfs", str(root), "tmpfs", MS_NOSUID | MS_NODEV, "mode=0755,size=16m")
    client_path = root / "usr/local/bin/cc-connect-file"
    client_path.parent.mkdir(parents=True)
    client_path.write_bytes(state["client"].encode("utf-8"))
    if hashlib.sha256(client_path.read_bytes()).hexdigest() != state["client_sha256"]:
        raise Refused("file client copy verification failed")
    client_path.chmod(0o555)
    for value in ("tmp", "home/model"):
        destination = root / value
        destination.mkdir(parents=True, exist_ok=True)
        mount("tmpfs", str(destination), "tmpfs", MS_NOSUID | MS_NODEV, "mode=0700,size=64m")
    for fd, target, directory, readonly in state["mounts"]:
        destination = root / target.lstrip("/")
        destination.parent.mkdir(parents=True, exist_ok=True)
        if directory:
            destination.mkdir(exist_ok=True)
        else:
            destination.touch(exist_ok=True)
        mount(f"/proc/self/fd/{fd}", str(destination), flags=MS_BIND)
        flags = MS_BIND | MS_REMOUNT | MS_NOSUID
        if not target.startswith("/dev/"):
            flags |= MS_NODEV
        mount(None, str(destination), flags=flags | (MS_RDONLY if readonly else 0))
    # Support merged-/usr distributions without mounting any host /etc, /home,
    # /var, /run or /tmp directory.
    for alias in ("bin", "lib", "lib64"):
        if not (root / alias).exists() and (root / "usr" / alias).exists():
            (root / alias).symlink_to("usr/" + alias)
    (root / "proc").mkdir()
    mount("proc", str(root / "proc"), "proc", MS_RDONLY | MS_NOSUID | MS_NODEV)
    (root / ".oldroot").mkdir()
    os.chdir(root)
    # Linux has no libc pivot_root wrapper. Fail closed on other architectures.
    number = {"x86_64": 155, "aarch64": 41}.get(platform.machine())
    if number is None:
        raise Refused("unsupported Linux architecture")
    libc = ctypes.CDLL(None, use_errno=True)
    if libc.syscall(number, b".", b".oldroot") != 0:
        raise Refused("pivot_root unavailable")
    os.chdir("/")
    if libc.umount2(b"/.oldroot", 2) != 0:
        raise Refused("host root detach failed")
    os.rmdir("/.oldroot")
    mount(None, "/", flags=MS_REMOUNT | MS_RDONLY | MS_NOSUID | MS_NODEV)
    os.closerange(3, os.sysconf("SC_OPEN_MAX"))
    os.chdir(state["work"])
    env = {"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": "/home/model", "TMPDIR": "/tmp",
           "LANG": "C.UTF-8", "PYTHONDONTWRITEBYTECODE": "1",
           "CC_ACTION_TOOLS_SOCKET": SOCKET_PATH}
    for name in CAPABILITY_ENV:
        if os.environ.get(name):
            env[name] = os.environ[name]
    os.execve("/usr/bin/setpriv", ["setpriv", "--no-new-privs", "--bounding-set=-all",
              "--inh-caps=-all", "--ambient-caps=-all", "--"] + state["command"], env)


def launch(args):
    if sys.platform != "linux":
        raise Refused("Linux namespace support required")
    if not Path("/usr/bin/unshare").is_file() or not Path("/usr/bin/setpriv").is_file():
        raise Refused("existing unshare and setpriv required")
    if any(not stat.S_ISFIFO(os.fstat(fd).st_mode) for fd in (0, 1, 2)):
        raise Refused("standard streams must be pipes")
    fds = []
    try:
        state = prepare(args.config, args.work_root, args.command, fds)
        with tempfile.TemporaryDirectory(prefix="cc-file-work-") as root:
            state["root"] = root
            state_fd = os.memfd_create("cc-file-work", os.MFD_CLOEXEC)
            fds.append(state_fd)
            os.write(state_fd, json.dumps(state).encode())
            os.lseek(state_fd, 0, os.SEEK_SET)
            env = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "PYTHONDONTWRITEBYTECODE": "1"}
            for name in CAPABILITY_ENV:
                if os.environ.get(name):
                    env[name] = os.environ[name]
            parent_pid = os.getpid()

            def die_with_supervisor():
                libc = ctypes.CDLL(None, use_errno=True)
                if libc.prctl(1, signal.SIGKILL, 0, 0, 0) != 0 or os.getppid() != parent_pid:
                    os._exit(78)

            child = subprocess.Popen(["/usr/bin/unshare", "--user", "--map-root-user", "--mount",
                "--pid", "--fork", "--kill-child=KILL", "--mount-proc", "--net", "--ipc", "--uts",
                sys.executable, "-I", "-B", str(Path(__file__).resolve()), "--inside", str(state_fd)],
                pass_fds=fds, env=env, preexec_fn=die_with_supervisor)
            # A killed supervisor kills unshare, whose PDEATHSIG kills PID 1
            # and hence all remaining processes in the namespace.
            previous = signal.signal(signal.SIGTERM, lambda *_: child.kill())
            try:
                return child.wait()
            finally:
                signal.signal(signal.SIGTERM, previous)
    finally:
        for fd in fds:
            os.close(fd)


def main():
    if len(sys.argv) == 3 and sys.argv[1] == "--inside":
        inside(int(sys.argv[2]))
        return 0
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True)
    parser.add_argument("--work-root", required=True)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.command[:1] == ["--"]:
        args.command = args.command[1:]
    return launch(args)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Refused as error:
        print("file-work sandbox refused: " + str(error), file=sys.stderr)
        sys.exit(78)
    except (OSError, ValueError, KeyError, TypeError):
        # Never log config paths, CLI arguments, socket contents or credentials.
        print("file-work sandbox refused: configuration or isolation unavailable", file=sys.stderr)
        sys.exit(78)

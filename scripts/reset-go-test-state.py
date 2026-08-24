#!/usr/bin/env python3
"""Back up OpenClaw state, verify a native build, and restart systemd fresh."""

from __future__ import annotations

import argparse
import hashlib
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

ROOT_DIR = Path(__file__).resolve().parent.parent
GO_DIR = ROOT_DIR / "golang"
CONFIG_DIR = ROOT_DIR / "config_test"
DATA_DIR = CONFIG_DIR / "agent_data_go"
BACKUP_DIR = CONFIG_DIR / "sqlite_backups"
DB_FILE = DATA_DIR / "openclaw-agent.sqlite"
BINARY_FILE = ROOT_DIR / "openclaw"
SERVICE = "openclaw-go.service"
GATEWAY_HEALTH_URL = "http://127.0.0.1:18792/healthz"
SQLITE_SIDECARS = ("-wal", "-shm", "-journal")


class ResetError(RuntimeError):
    """Raised when a reset safety check or operation fails."""


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--yes",
        action="store_true",
        help="confirm replacement of conversations, memories, tasks, reminders, and traces",
    )
    args = parser.parse_args()
    if not args.yes:
        parser.error(
            "--yes is required because this operation replaces all application state"
        )
    return args


def run_command(
    arguments: list[str],
    *,
    cwd: Path | None = None,
    env: dict[str, str] | None = None,
    capture_output: bool = False,
) -> str:
    try:
        result = subprocess.run(
            arguments,
            cwd=cwd,
            env=env,
            check=True,
            capture_output=capture_output,
            text=True,
        )
    except subprocess.CalledProcessError as error:
        detail = ""
        if capture_output:
            detail = (error.stderr or "").strip() or (error.stdout or "").strip()
        suffix = f": {detail}" if detail else f" with status {error.returncode}"
        raise ResetError(f"{' '.join(arguments)} failed{suffix}") from error
    return result.stdout.strip() if capture_output else ""


def systemctl(*arguments: str, capture_output: bool = False) -> str:
    command = ["systemctl", *arguments]
    if os.geteuid() != 0:
        command.insert(0, "sudo")
    return run_command(command, capture_output=capture_output)


def require_regular_file(path: Path, description: str) -> None:
    if path.is_symlink() or not path.is_file():
        raise ResetError(f"{description} is missing or is a symbolic link: {path}")


def require_commands() -> None:
    for command in ("git", "go", "gofmt", "python3", "systemctl"):
        if shutil.which(command) is None:
            raise ResetError(f"required command not found: {command}")
    if os.geteuid() != 0 and shutil.which("sudo") is None:
        raise ResetError("required command not found: sudo")


def integrity_check(path: Path) -> None:
    uri = f"{path.resolve().as_uri()}?mode=ro"
    try:
        with sqlite3.connect(uri, uri=True, timeout=10) as database:
            rows = database.execute("PRAGMA integrity_check").fetchall()
    except sqlite3.Error as error:
        raise ResetError(f"could not inspect SQLite database {path}: {error}") from error
    if rows != [("ok",)]:
        details = "; ".join(str(row[0]) for row in rows)
        raise ResetError(f"SQLite integrity check failed for {path}: {details}")


def backup_database(source: Path, destination: Path) -> None:
    require_regular_file(source, "canonical SQLite database")
    if destination.exists() or destination.is_symlink():
        raise ResetError(f"backup already exists: {destination}")

    source_uri = f"{source.resolve().as_uri()}?mode=ro"
    try:
        with sqlite3.connect(source_uri, uri=True, timeout=10) as original:
            with sqlite3.connect(destination, timeout=10) as backup:
                original.backup(backup)
    except (OSError, sqlite3.Error) as error:
        if destination.is_file() and not destination.is_symlink():
            destination.unlink()
        raise ResetError(f"could not back up SQLite database {source}: {error}") from error

    destination.chmod(0o600)
    require_regular_file(destination, "backup SQLite database")
    integrity_check(destination)


def file_digest(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def go_environment() -> dict[str, str]:
    environment = os.environ.copy()
    environment["CGO_ENABLED"] = "1"
    environment.setdefault("GOCACHE", "/tmp/openclaw-go-cache")
    return environment


def build_candidate_binary(destination: Path) -> None:
    if GO_DIR.is_symlink() or not GO_DIR.is_dir():
        raise ResetError(f"Go build context is missing or is a symbolic link: {GO_DIR}")

    print(f"Building candidate binary from: {GO_DIR}")
    run_command(
        [
            "go",
            "build",
            "-tags",
            "sqlite_fts5",
            "-o",
            str(destination),
            "./cmd/openclaw",
        ],
        cwd=GO_DIR,
        env=go_environment(),
    )
    require_regular_file(destination, "candidate OpenClaw binary")
    if destination.stat().st_mode & 0o111 == 0:
        raise ResetError(f"candidate OpenClaw binary is not executable: {destination}")
    print(f"Candidate binary SHA-256: {file_digest(destination)}")


def run_verification() -> None:
    environment = go_environment()
    print("Running reset-helper tests, Go tests, race tests, and vet")
    run_command(
        ["python3", "-m", "unittest", "scripts/test_reset_go_test_state.py"],
        cwd=ROOT_DIR,
    )
    commands = (
        ["go", "test", "-tags", "sqlite_fts5", "./..."],
        ["go", "test", "-tags", "sqlite_fts5", "-race", "./..."],
        ["go", "vet", "-tags", "sqlite_fts5", "./..."],
    )
    for command in commands:
        run_command(command, cwd=GO_DIR, env=environment)

    unformatted = run_command(["gofmt", "-l", "."], cwd=GO_DIR, capture_output=True)
    if unformatted:
        raise ResetError(f"gofmt is required for:\n{unformatted}")
    run_command(["git", "diff", "--check"], cwd=ROOT_DIR)


def install_candidate_binary(candidate: Path) -> None:
    require_regular_file(candidate, "candidate OpenClaw binary")
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=".openclaw-install-", dir=ROOT_DIR
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as destination, candidate.open("rb") as source:
            shutil.copyfileobj(source, destination)
            destination.flush()
            os.fsync(destination.fileno())
        temporary.chmod(0o755)
        temporary.replace(BINARY_FILE)
    except BaseException:
        if temporary.exists() and not temporary.is_symlink():
            temporary.unlink()
        raise

    require_regular_file(BINARY_FILE, "installed OpenClaw binary")
    if file_digest(BINARY_FILE) != file_digest(candidate):
        raise ResetError("installed OpenClaw binary does not match the candidate build")


def service_active() -> bool:
    try:
        return systemctl("is-active", SERVICE, capture_output=True) == "active"
    except ResetError:
        return False


def validate_service_configuration() -> None:
    details = systemctl(
        "show",
        SERVICE,
        "--property=WorkingDirectory",
        "--property=ExecStart",
        "--property=Environment",
        capture_output=True,
    )
    properties = {}
    for line in details.splitlines():
        key, separator, value = line.partition("=")
        if separator:
            properties[key] = value

    expected_environment = (
        f"OPENCLAW_DATA_DIR={DATA_DIR}",
        f"OPENCLAW_CONFIG_DIR={CONFIG_DIR}",
        "PORT=18792",
    )
    if properties.get("WorkingDirectory") != str(ROOT_DIR):
        raise ResetError(
            f"{SERVICE} has unexpected WorkingDirectory: "
            f"{properties.get('WorkingDirectory', '')}"
        )
    if f"path={BINARY_FILE} ;" not in properties.get("ExecStart", ""):
        raise ResetError(f"{SERVICE} does not execute repository binary: {BINARY_FILE}")
    environment = properties.get("Environment", "")
    for expected in expected_environment:
        if expected not in environment.split():
            raise ResetError(f"{SERVICE} is missing expected environment: {expected}")
    if not service_active():
        raise ResetError(f"systemd service is not active: {SERVICE}")


def service_ready() -> bool:
    if not service_active():
        return False
    try:
        with urllib.request.urlopen(GATEWAY_HEALTH_URL, timeout=2) as response:
            return response.status == 200 and response.read().decode().strip() == "OK"
    except (OSError, UnicodeError, urllib.error.URLError):
        return False


def wait_for_fresh_runtime(timeout_seconds: int = 30) -> None:
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        if DB_FILE.is_file() and not DB_FILE.is_symlink() and service_ready():
            return
        time.sleep(1)
    if not service_active():
        raise ResetError(f"{SERVICE} did not remain active after the reset")
    if not DB_FILE.is_file() or DB_FILE.is_symlink():
        raise ResetError("OpenClaw did not create a fresh canonical SQLite database")
    raise ResetError("OpenClaw created its database but did not become healthy")


def assert_fresh_database(path: Path) -> None:
    tables = (
        "memories",
        "memory_revisions",
        "conversation_history",
        "tasks",
        "reminders",
        "response_traces",
    )
    uri = f"{path.resolve().as_uri()}?mode=ro"
    try:
        with sqlite3.connect(uri, uri=True, timeout=10) as database:
            for table in tables:
                count = database.execute(f"SELECT count(*) FROM {table}").fetchone()[0]
                if count != 0:
                    raise ResetError(
                        f"fresh database unexpectedly contains {count} rows in {table}"
                    )
    except sqlite3.Error as error:
        raise ResetError(f"could not verify fresh SQLite database {path}: {error}") from error


def archive_failed_fresh_database(reset_id: str) -> Path | None:
    members = [DB_FILE, *(Path(f"{DB_FILE}{suffix}") for suffix in SQLITE_SIDECARS)]
    existing = [path for path in members if path.exists() or path.is_symlink()]
    if not existing:
        return None

    failed_dir = BACKUP_DIR / f"failed-reset-{reset_id}-{os.getpid()}"
    failed_dir.mkdir(mode=0o700)
    for path in existing:
        path.rename(failed_dir / path.name)
    return failed_dir


def reset_state() -> None:
    require_commands()
    if DATA_DIR.is_symlink() or not DATA_DIR.is_dir():
        raise ResetError(f"data directory is missing or is a symbolic link: {DATA_DIR}")
    require_regular_file(CONFIG_DIR / "openclaw.json", "test configuration")
    require_regular_file(CONFIG_DIR / "secrets.json", "test secrets")
    require_regular_file(DB_FILE, "canonical SQLite database")
    require_regular_file(BINARY_FILE, "current OpenClaw binary")
    validate_service_configuration()

    BACKUP_DIR.mkdir(mode=0o700, parents=True, exist_ok=True)
    if BACKUP_DIR.is_symlink() or not BACKUP_DIR.is_dir():
        raise ResetError(f"backup directory is not a regular directory: {BACKUP_DIR}")

    reset_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    backup_file = BACKUP_DIR / f"openclaw-agent.sqlite.before-reset-{reset_id}"
    previous_binary = BACKUP_DIR / f"openclaw.before-reset-{reset_id}"
    retired_file = DATA_DIR / f".openclaw-agent.sqlite.retired-{reset_id}-{os.getpid()}"
    if backup_file.exists() or backup_file.is_symlink():
        raise ResetError(f"backup already exists: {backup_file}")
    if previous_binary.exists() or previous_binary.is_symlink():
        raise ResetError(f"binary backup already exists: {previous_binary}")
    if retired_file.exists() or retired_file.is_symlink():
        raise ResetError(f"temporary retired database already exists: {retired_file}")

    service_stopped = False
    backup_created = False
    binary_installed = False
    database_retired = False

    try:
        print(f"Stopping systemd service: {SERVICE}")
        systemctl("stop", SERVICE)
        service_stopped = True

        for suffix in SQLITE_SIDECARS:
            sidecar = Path(f"{DB_FILE}{suffix}")
            if sidecar.exists() or sidecar.is_symlink():
                raise ResetError(
                    f"SQLite sidecar remains after shutdown; database was not reset: {sidecar}"
                )

        integrity_check(DB_FILE)
        print(f"Backing up database: {backup_file}")
        backup_database(DB_FILE, backup_file)
        backup_created = True

        with tempfile.TemporaryDirectory(prefix="openclaw-go-reset-") as temporary:
            temporary_dir = Path(temporary)
            candidate_binary = temporary_dir / "openclaw-candidate"

            build_candidate_binary(candidate_binary)
            run_verification()

            shutil.copy2(BINARY_FILE, previous_binary)
            previous_binary.chmod(0o755)
            require_regular_file(previous_binary, "previous OpenClaw binary")
            print(f"Installing candidate binary: {BINARY_FILE}")
            install_candidate_binary(candidate_binary)
            binary_installed = True

            print(f"Retiring canonical database before fresh startup: {DB_FILE}")
            DB_FILE.rename(retired_file)
            database_retired = True

            print(f"Starting systemd service with fresh state: {SERVICE}")
            systemctl("start", SERVICE)
            service_stopped = False
            wait_for_fresh_runtime()
            integrity_check(DB_FILE)
            assert_fresh_database(DB_FILE)

            retired_file.unlink()
            database_retired = False
    except (Exception, KeyboardInterrupt):
        print("Reset did not complete; restoring the previous runtime.", file=sys.stderr)
        try:
            if database_retired:
                try:
                    systemctl("stop", SERVICE)
                except ResetError:
                    pass
                failed_dir = archive_failed_fresh_database(reset_id)
                if failed_dir is not None:
                    print(f"Failed fresh database preserved at: {failed_dir}", file=sys.stderr)
                retired_file.rename(DB_FILE)
                database_retired = False

            if binary_installed:
                install_candidate_binary(previous_binary)
                binary_installed = False

            if service_stopped or not service_active():
                systemctl("start", SERVICE)
                wait_for_fresh_runtime()
        except (OSError, ResetError) as recovery_error:
            print(f"error restoring previous runtime: {recovery_error}", file=sys.stderr)
        if backup_created:
            print(f"SQLite backup preserved at: {backup_file}", file=sys.stderr)
        raise

    print(
        f"Reset complete.\n"
        f"Service: {SERVICE}\n"
        f"Installed binary: {BINARY_FILE}\n"
        f"Fresh database: {DB_FILE}\n"
        f"Backup database: {backup_file}\n"
        f"Previous binary: {previous_binary}"
    )


def main() -> int:
    parse_args()
    try:
        reset_state()
    except KeyboardInterrupt:
        print("error: interrupted", file=sys.stderr)
        return 130
    except (OSError, ResetError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

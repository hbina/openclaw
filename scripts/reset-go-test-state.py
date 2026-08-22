#!/usr/bin/env python3
"""Build OpenClaw, archive the Go test state, and restart it fresh."""

from __future__ import annotations

import argparse
import hashlib
import re
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path

ROOT_DIR = Path(__file__).resolve().parent.parent
GO_DIR = ROOT_DIR / "golang"
DATA_DIR = ROOT_DIR / "config_test" / "agent_data_go"
BACKUP_DIR = ROOT_DIR / "config_test" / "sqlite_backups"
DB_FILE = DATA_DIR / "openclaw-agent.sqlite"
CONTAINER = "openclaw-go-test-ubuntu"
CANDIDATE_IMAGE = "openclaw-go-ubuntu-test:reset-candidate"
CONTAINER_BINARY = "/app/openclaw"


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


def docker(*arguments: str) -> str:
    try:
        result = subprocess.run(
            ["docker", *arguments],
            check=True,
            capture_output=True,
            text=True,
        )
    except subprocess.CalledProcessError as error:
        detail = error.stderr.strip() or error.stdout.strip() or "no diagnostic output"
        raise ResetError(f"docker {' '.join(arguments)} failed: {detail}") from error
    return result.stdout.strip()


def docker_stream(*arguments: str) -> None:
    try:
        subprocess.run(["docker", *arguments], check=True)
    except subprocess.CalledProcessError as error:
        raise ResetError(
            f"docker {' '.join(arguments)} failed with status {error.returncode}"
        ) from error


def require_regular_file(path: Path, description: str) -> None:
    if path.is_symlink() or not path.is_file():
        raise ResetError(f"{description} is missing or is a symbolic link: {path}")


def integrity_check(path: Path) -> None:
    uri = f"{path.as_uri()}?mode=ro"
    try:
        with sqlite3.connect(uri, uri=True, timeout=10) as database:
            rows = database.execute("PRAGMA integrity_check").fetchall()
    except sqlite3.Error as error:
        raise ResetError(
            f"could not inspect SQLite database {path}: {error}"
        ) from error
    if rows != [("ok",)]:
        details = "; ".join(str(row[0]) for row in rows)
        raise ResetError(f"SQLite integrity check failed for {path}: {details}")


def file_digest(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def build_candidate_binary(destination: Path) -> None:
    if GO_DIR.is_symlink() or not GO_DIR.is_dir():
        raise ResetError(f"Go build context is missing or is a symbolic link: {GO_DIR}")

    print(f"Building candidate image from: {GO_DIR}")
    docker_stream("build", "--tag", CANDIDATE_IMAGE, str(GO_DIR))

    extraction_container = docker("create", CANDIDATE_IMAGE)
    if re.fullmatch(r"[0-9a-f]{64}", extraction_container) is None:
        raise ResetError(
            f"docker create returned an invalid container id: {extraction_container!r}"
        )
    try:
        docker("cp", f"{extraction_container}:{CONTAINER_BINARY}", str(destination))
    finally:
        docker("rm", extraction_container)

    require_regular_file(destination, "candidate OpenClaw binary")
    if destination.stat().st_mode & 0o111 == 0:
        raise ResetError(f"candidate OpenClaw binary is not executable: {destination}")
    print(f"Candidate binary SHA-256: {file_digest(destination)}")


def install_candidate_binary(candidate: Path, verification: Path) -> None:
    docker("cp", str(candidate), f"{CONTAINER}:{CONTAINER_BINARY}")
    docker("cp", f"{CONTAINER}:{CONTAINER_BINARY}", str(verification))
    require_regular_file(verification, "installed OpenClaw binary")
    if file_digest(verification) != file_digest(candidate):
        raise ResetError("installed OpenClaw binary does not match the candidate build")


def container_running() -> bool:
    return docker("inspect", CONTAINER, "--format", "{{.State.Running}}") == "true"


def container_ready() -> bool:
    state = docker(
        "inspect",
        CONTAINER,
        "--format",
        "{{.State.Running}} {{.State.Restarting}}",
    )
    if state != "true false":
        return False
    try:
        response = docker(
            "exec",
            CONTAINER,
            "wget",
            "-qO-",
            "http://127.0.0.1:18789/healthz",
        )
    except ResetError:
        return False
    return response == "OK"


def mounted_data_source() -> Path:
    source = docker(
        "inspect",
        CONTAINER,
        "--format",
        '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Source}}{{end}}{{end}}',
    )
    if not source:
        raise ResetError(f"container has no /data mount: {CONTAINER}")
    return Path(source)


def wait_for_fresh_runtime(timeout_seconds: int = 30) -> None:
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        if (
            DB_FILE.is_file()
            and not DB_FILE.is_symlink()
            and container_ready()
        ):
            return
        time.sleep(1)
    if not container_running():
        raise ResetError("container did not remain running after the reset")
    if not DB_FILE.is_file() or DB_FILE.is_symlink():
        raise ResetError("OpenClaw did not create a fresh canonical SQLite database")
    raise ResetError("OpenClaw created its database but did not become healthy")


def reset_state() -> None:
    if shutil.which("docker") is None:
        raise ResetError("required command not found: docker")
    if DATA_DIR.is_symlink() or not DATA_DIR.is_dir():
        raise ResetError(f"data directory is missing or is a symbolic link: {DATA_DIR}")
    require_regular_file(DB_FILE, "canonical SQLite database")

    BACKUP_DIR.mkdir(mode=0o700, parents=True, exist_ok=True)
    if BACKUP_DIR.is_symlink() or not BACKUP_DIR.is_dir():
        raise ResetError(f"backup directory is not a regular directory: {BACKUP_DIR}")

    reset_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    backup_file = BACKUP_DIR / f"openclaw-agent.sqlite.before-reset-{reset_id}"
    if backup_file.exists() or backup_file.is_symlink():
        raise ResetError(f"backup already exists: {backup_file}")

    docker("container", "inspect", CONTAINER, "--format", "{{.Id}}")
    if not container_running():
        raise ResetError(f"container is not running: {CONTAINER}")
    data_source = mounted_data_source()
    if data_source != DATA_DIR:
        raise ResetError(f"unexpected /data source for {CONTAINER}: {data_source}")

    with tempfile.TemporaryDirectory(prefix="openclaw-go-reset-") as temporary:
        temporary_dir = Path(temporary)
        candidate_binary = temporary_dir / "openclaw-candidate"
        installed_binary = temporary_dir / "openclaw-installed"
        previous_binary = temporary_dir / "openclaw-previous"
        build_candidate_binary(candidate_binary)

        container_stopped = False
        database_archived = False
        binary_install_attempted = False
        try:
            print(f"Stopping container: {CONTAINER}")
            docker("stop", CONTAINER)
            container_stopped = True

            for suffix in ("-wal", "-shm", "-journal"):
                sidecar = Path(f"{DB_FILE}{suffix}")
                if sidecar.exists() or sidecar.is_symlink():
                    raise ResetError(
                        f"SQLite sidecar remains after shutdown; database was not archived: {sidecar}"
                    )

            integrity_check(DB_FILE)
            docker("cp", f"{CONTAINER}:{CONTAINER_BINARY}", str(previous_binary))
            require_regular_file(previous_binary, "previous OpenClaw binary")
            print(f"Installing candidate binary into: {CONTAINER}")
            binary_install_attempted = True
            install_candidate_binary(candidate_binary, installed_binary)
            print(f"Archiving database: {backup_file}")
            DB_FILE.rename(backup_file)
            database_archived = True

            require_regular_file(backup_file, "archived SQLite database")
            integrity_check(backup_file)
            if DB_FILE.exists() or DB_FILE.is_symlink():
                raise ResetError(
                    f"canonical database path still exists after archival: {DB_FILE}"
                )

            print(f"Starting container with fresh state: {CONTAINER}")
            docker("start", CONTAINER)
            container_stopped = False
            wait_for_fresh_runtime()
            integrity_check(DB_FILE)
        except (Exception, KeyboardInterrupt):
            if database_archived:
                print(
                    f"Reset did not complete. The prior database is preserved at:\n{backup_file}",
                    file=sys.stderr,
                )
            elif container_stopped:
                print(
                    "Reset failed before archival; restarting the container.",
                    file=sys.stderr,
                )
                try:
                    if binary_install_attempted:
                        docker(
                            "cp",
                            str(previous_binary),
                            f"{CONTAINER}:{CONTAINER_BINARY}",
                        )
                    docker("start", CONTAINER)
                except ResetError as restart_error:
                    print(f"error: {restart_error}", file=sys.stderr)
            raise

    print(
        f"Reset complete.\n"
        f"Candidate image: {CANDIDATE_IMAGE}\n"
        f"Fresh database: {DB_FILE}\n"
        f"Backup database: {backup_file}"
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

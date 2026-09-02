#!/usr/bin/env python3
"""Build, verify, and cut over to the Go container with fresh SQLite state."""

from __future__ import annotations

import argparse
import json
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
CONFIG_FILE = CONFIG_DIR / "openclaw.json"
SECRETS_FILE = CONFIG_DIR / "secrets.json"
DATA_DIR = CONFIG_DIR / "agent_data_go"
BACKUP_DIR = CONFIG_DIR / "docker_cutover_backups"
DB_FILE = DATA_DIR / "openclaw-agent.sqlite"

IMAGE_REPOSITORY = "openclaw-go"
DEFAULT_TARGET_CONTAINER = "openclaw-go"
DEFAULT_HOST_PORT = 18792
CONTAINER_PORT = 18789
SQLITE_SIDECARS = ("-wal", "-shm", "-journal")


class DeployError(RuntimeError):
    """Raised when a cutover safety check or operation fails."""


def parse_args(arguments: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="operation", required=True)

    cutover = subparsers.add_parser("cutover", help="perform a fresh-state cutover")
    cutover.add_argument("--yes", action="store_true")
    cutover.add_argument("--previous-container", required=True)
    cutover.add_argument("--target-container", default=DEFAULT_TARGET_CONTAINER)
    cutover.add_argument("--host-port", type=int, default=DEFAULT_HOST_PORT)
    cutover.add_argument("--tag", default="")

    rollback = subparsers.add_parser("rollback", help="restore a retained pre-cutover runtime")
    rollback.add_argument("--yes", action="store_true")
    rollback.add_argument("--previous-container", required=True)
    rollback.add_argument("--target-container", default=DEFAULT_TARGET_CONTAINER)
    rollback.add_argument("--retired-data", required=True)

    args = parser.parse_args(arguments)
    if not args.yes:
        parser.error("--yes is required because this operation changes the active runtime and state directory")
    if args.previous_container == args.target_container:
        parser.error("the previous and target container names must differ")
    if args.operation == "cutover" and not 1 <= args.host_port <= 65535:
        parser.error("--host-port must be between 1 and 65535")
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
        raise DeployError(f"{' '.join(arguments)} failed{suffix}") from error
    return result.stdout.strip() if capture_output else ""


def docker(*arguments: str, capture_output: bool = False) -> str:
    return run_command(["docker", *arguments], capture_output=capture_output)


def require_commands() -> None:
    for command in ("docker", "git", "go", "gofmt", "python3"):
        if shutil.which(command) is None:
            raise DeployError(f"required command not found: {command}")


def require_regular_file(path: Path, description: str) -> None:
    if path.is_symlink() or not path.is_file():
        raise DeployError(f"{description} is missing or is a symbolic link: {path}")


def require_directory(path: Path, description: str) -> None:
    if path.is_symlink() or not path.is_dir():
        raise DeployError(f"{description} is missing or is a symbolic link: {path}")


def validate_cutover_configuration() -> None:
    try:
        public = json.loads(CONFIG_FILE.read_text())
        secrets = json.loads(SECRETS_FILE.read_text())
    except (OSError, UnicodeError, json.JSONDecodeError) as error:
        raise DeployError(f"could not validate cutover configuration: {error}") from error
    telegram = public.get("channels", {}).get("telegram", {})
    owner_id = str(telegram.get("ownerUserId", "")).strip()
    try:
        numeric_owner = int(owner_id)
    except ValueError:
        numeric_owner = 0
    if telegram.get("enabled") is not True or numeric_owner <= 0:
        raise DeployError("cutover requires enabled Telegram with one positive numeric ownerUserId")
    maintenance = public.get("agents", {}).get("defaults", {}).get("memoryMaintenance", {})
    if maintenance.get("enabled") is not True:
        raise DeployError("cutover requires agents.defaults.memoryMaintenance.enabled=true")
    bot_token = secrets.get("channels", {}).get("telegram", {}).get("botToken", "")
    if not isinstance(bot_token, str) or not bot_token.strip():
        raise DeployError("cutover requires a Telegram bot token in secrets.json")


def container_exists(name: str) -> bool:
    return docker_object_exists("container", name)


def image_exists(name: str) -> bool:
    return docker_object_exists("image", name)


def docker_object_exists(kind: str, name: str) -> bool:
    result = subprocess.run(
        ["docker", kind, "inspect", name],
        check=False,
        capture_output=True,
        text=True,
    )
    if result.returncode == 0:
        return True
    detail = (result.stderr or result.stdout).strip()
    if result.returncode == 1 and ("No such" in detail or "not found" in detail.lower()):
        return False
    raise DeployError(f"docker {kind} inspect {name} failed: {detail or result.returncode}")


def container_running(name: str) -> bool:
    if not container_exists(name):
        return False
    return docker("inspect", name, "--format", "{{.State.Running}}", capture_output=True) == "true"


def wait_for_container(name: str, should_run: bool, timeout_seconds: int = 30) -> None:
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        if container_running(name) is should_run:
            return
        time.sleep(0.25)
    expectation = "running" if should_run else "stopped"
    raise DeployError(f"container {name} did not become {expectation}")


def health_url(host_port: int) -> str:
    return f"http://127.0.0.1:{host_port}/healthz"


def wait_for_gateway(host_port: int, timeout_seconds: int = 45) -> None:
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(health_url(host_port), timeout=2) as response:
                if response.status == 200 and response.read().decode().strip() == "OK":
                    return
        except (OSError, UnicodeError, urllib.error.URLError):
            pass
        time.sleep(0.5)
    raise DeployError(f"Gateway did not become healthy at {health_url(host_port)}")


def integrity_check(path: Path) -> None:
    require_regular_file(path, "SQLite database")
    uri = f"{path.resolve().as_uri()}?mode=ro"
    try:
        with sqlite3.connect(uri, uri=True, timeout=10) as database:
            rows = database.execute("PRAGMA integrity_check").fetchall()
    except sqlite3.Error as error:
        raise DeployError(f"could not inspect SQLite database {path}: {error}") from error
    if rows != [("ok",)]:
        raise DeployError(f"SQLite integrity check failed for {path}: {rows}")


def backup_database(source: Path, destination: Path) -> None:
    require_regular_file(source, "canonical SQLite database")
    if destination.exists() or destination.is_symlink():
        raise DeployError(f"backup already exists: {destination}")
    source_uri = f"{source.resolve().as_uri()}?mode=ro"
    try:
        with sqlite3.connect(source_uri, uri=True, timeout=10) as original:
            with sqlite3.connect(destination, timeout=10) as backup:
                original.backup(backup)
    except (OSError, sqlite3.Error) as error:
        if destination.is_file() and not destination.is_symlink():
            destination.unlink()
        raise DeployError(f"could not back up SQLite database {source}: {error}") from error
    destination.chmod(0o600)
    integrity_check(destination)


def assert_fresh_database(path: Path) -> None:
    tables = (
        "memories",
        "memory_revisions",
        "memory_candidates",
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
                    raise DeployError(f"fresh database unexpectedly contains {count} rows in {table}")
            checkpoint = database.execute(
                "SELECT checkpoint_history_id FROM memory_maintenance_state WHERE singleton_id=1"
            ).fetchone()
            if checkpoint != (0,):
                raise DeployError(f"fresh database has invalid maintenance checkpoint: {checkpoint}")
    except sqlite3.Error as error:
        raise DeployError(f"could not verify fresh SQLite database {path}: {error}") from error


def assert_smoke_persisted(path: Path, sender_id: str) -> None:
    uri = f"{path.resolve().as_uri()}?mode=ro"
    try:
        with sqlite3.connect(uri, uri=True, timeout=10) as database:
            completed_traces = database.execute(
                "SELECT count(*) FROM response_traces WHERE channel_id='cli' AND sender_id=? AND status='completed'",
                (sender_id,),
            ).fetchone()[0]
            conversation_rows = database.execute(
                "SELECT count(*) FROM conversation_history WHERE channel_id='cli' AND sender_id=?",
                (sender_id,),
            ).fetchone()[0]
            chunk_count = database.execute("SELECT count(*) FROM conversation_chunks").fetchone()[0]
            unwanted = sum(
                database.execute(f"SELECT count(*) FROM {table}").fetchone()[0]
                for table in ("memories", "tasks", "reminders")
            )
    except sqlite3.Error as error:
        raise DeployError(f"could not verify persisted HTTP smoke turn: {error}") from error
    if completed_traces != 1 or conversation_rows != 2 or chunk_count < 1:
        raise DeployError(
            "HTTP smoke turn did not atomically persist one trace, one complete exchange, and its recall chunk"
        )
    if unwanted != 0:
        raise DeployError("HTTP smoke turn unexpectedly mutated memory, task, or reminder state")


def go_environment() -> dict[str, str]:
    environment = os.environ.copy()
    environment["CGO_ENABLED"] = "1"
    environment.setdefault("GOCACHE", "/tmp/openclaw-go-cache")
    return environment


def run_verification() -> None:
    environment = go_environment()
    run_command(
        [
            "python3",
            "-m",
            "unittest",
            "scripts/test_deploy_go_docker.py",
            "scripts/test_reset_go_test_state.py",
        ],
        cwd=ROOT_DIR,
    )
    commands = (
        ["go", "test", "-tags", "sqlite_fts5", "./..."],
        ["go", "test", "-race", "-tags", "sqlite_fts5", "./..."],
        ["go", "vet", "-tags", "sqlite_fts5", "./..."],
    )
    for command in commands:
        run_command(command, cwd=GO_DIR, env=environment)
    unformatted = run_command(["gofmt", "-l", "."], cwd=GO_DIR, capture_output=True)
    if unformatted:
        raise DeployError(f"gofmt is required for:\n{unformatted}")
    run_command(["git", "diff", "--check"], cwd=ROOT_DIR)


def build_image(image: str) -> None:
    run_command(["docker", "build", "-t", image, str(GO_DIR)])


def start_candidate(image: str, target: str, host_port: int) -> None:
    docker(
        "run",
        "-d",
        "--name",
        target,
        "--restart",
        "unless-stopped",
        "--network",
        "bridge",
        "-p",
        f"127.0.0.1:{host_port}:{CONTAINER_PORT}",
        "-e",
        "OPENCLAW_CONFIG_DIR=/config",
        "-e",
        "OPENCLAW_DATA_DIR=/data",
        "-e",
        "TZ=Asia/Kuala_Lumpur",
        "-v",
        f"{CONFIG_FILE}:/config/openclaw.json:ro",
        "-v",
        f"{SECRETS_FILE}:/config/secrets.json:ro",
        "-v",
        f"{DATA_DIR}:/data",
        image,
    )


def post_smoke_chat(host_port: int, sender_id: str) -> None:
    body = json.dumps(
        {
            "sender_id": sender_id,
            "message": "Reply briefly that the fresh Go deployment is ready. Do not create a task, reminder, or memory.",
        }
    ).encode()
    request = urllib.request.Request(
        f"http://127.0.0.1:{host_port}/chat",
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=600) as response:
            payload = json.loads(response.read())
            trace_id = response.headers.get("X-OpenClaw-Trace-ID", "")
    except (OSError, UnicodeError, ValueError, urllib.error.URLError) as error:
        raise DeployError(f"fresh Go HTTP smoke turn failed: {error}") from error
    if response.status != 200 or not isinstance(payload.get("reply"), str) or not payload["reply"].strip():
        raise DeployError("fresh Go HTTP smoke turn returned an invalid response")
    if not trace_id.isdigit() or int(trace_id) <= 0:
        raise DeployError("fresh Go HTTP smoke turn did not return a trace ID")


def unique_cutover_id() -> str:
    return datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + f"-{os.getpid()}"


def validate_common(previous: str, target: str) -> None:
    require_commands()
    require_directory(GO_DIR, "Go build directory")
    require_directory(CONFIG_DIR, "configuration directory")
    require_regular_file(CONFIG_FILE, "OpenClaw configuration")
    require_regular_file(SECRETS_FILE, "OpenClaw secrets")
    require_directory(DATA_DIR, "current Go data directory")
    validate_cutover_configuration()
    if not container_running(previous):
        raise DeployError(f"previous container is not running: {previous}")
    if container_exists(target):
        raise DeployError(f"target container already exists: {target}")


def archive_failed_data(cutover_id: str) -> Path | None:
    if not DATA_DIR.exists() and not DATA_DIR.is_symlink():
        return None
    destination = BACKUP_DIR / f"agent_data_go.failed-{cutover_id}"
    if destination.exists() or destination.is_symlink():
        raise DeployError(f"failed-data archive already exists: {destination}")
    DATA_DIR.rename(destination)
    return destination


def restore_after_failed_cutover(previous: str, target: str, retired_data: Path, cutover_id: str) -> None:
    recovery_errors: list[str] = []
    try:
        if container_exists(target):
            docker("rm", "-f", target)
    except DeployError as error:
        recovery_errors.append(str(error))
    try:
        failed = archive_failed_data(cutover_id)
        if failed is not None:
            print(f"Failed fresh data preserved at: {failed}", file=sys.stderr)
        retired_data.rename(DATA_DIR)
    except (OSError, DeployError) as error:
        recovery_errors.append(f"restore prior data directory: {error}")
    try:
        docker("start", previous)
        wait_for_container(previous, True)
    except DeployError as error:
        recovery_errors.append(f"restart previous container: {error}")
    if recovery_errors:
        raise DeployError("automatic rollback was incomplete: " + "; ".join(recovery_errors))


def cutover(args: argparse.Namespace) -> None:
    validate_common(args.previous_container, args.target_container)
    BACKUP_DIR.mkdir(mode=0o700, parents=True, exist_ok=True)
    require_directory(BACKUP_DIR, "Docker cutover backup directory")
    cutover_id = unique_cutover_id()
    tag = args.tag or f"cutover-{cutover_id}"
    if not all(character.isalnum() or character in "._-" for character in tag):
        raise DeployError("image tag contains unsupported characters")
    image = f"{IMAGE_REPOSITORY}:{tag}"
    if image_exists(image):
        raise DeployError(f"image tag already exists: {image}")

    retired_data = BACKUP_DIR / f"agent_data_go.before-{cutover_id}"
    database_backup = BACKUP_DIR / f"openclaw-agent.after-{cutover_id}.sqlite"
    rehearsal = BACKUP_DIR / f"openclaw-agent.rehearsal-{cutover_id}.sqlite"
    smoke_sender = f"docker-cutover-{cutover_id}"
    for artifact in (retired_data, database_backup, rehearsal):
        if artifact.exists() or artifact.is_symlink():
            raise DeployError(f"cutover artifact already exists: {artifact}")

    print("Running Go verification gates")
    run_verification()
    print(f"Building candidate image: {image}")
    build_image(image)

    previous_stopped = False
    data_retired = False
    try:
        print(f"Stopping retained previous container: {args.previous_container}")
        docker("stop", args.previous_container)
        previous_stopped = True
        wait_for_container(args.previous_container, False)

        for suffix in SQLITE_SIDECARS:
            sidecar = Path(f"{DB_FILE}{suffix}")
            if sidecar.exists() or sidecar.is_symlink():
                raise DeployError(f"SQLite sidecar remains after shutdown: {sidecar}")

        print(f"Archiving pre-ledger Go data: {retired_data}")
        DATA_DIR.rename(retired_data)
        data_retired = True
        DATA_DIR.mkdir(mode=0o700)

        print(f"Starting fresh Go container: {args.target_container}")
        start_candidate(image, args.target_container, args.host_port)
        wait_for_gateway(args.host_port)
        integrity_check(DB_FILE)
        assert_fresh_database(DB_FILE)
        docker(
            "exec",
            args.target_container,
            "./openclaw",
            "--check-state",
            "/data/openclaw-agent.sqlite",
        )

        print("Running one real local-model HTTP turn")
        post_smoke_chat(args.host_port, smoke_sender)
        integrity_check(DB_FILE)
        assert_smoke_persisted(DB_FILE, smoke_sender)

        print("Verifying restart persistence")
        docker("restart", args.target_container)
        wait_for_gateway(args.host_port)
        assert_smoke_persisted(DB_FILE, smoke_sender)

        print(f"Backing up fresh authoritative state: {database_backup}")
        backup_database(DB_FILE, database_backup)
        backup_database(database_backup, rehearsal)
        rehearsal_name = rehearsal.name
        docker(
            "run",
            "--rm",
            "-v",
            f"{BACKUP_DIR}:/recovery",
            image,
            "./openclaw",
            "--check-state",
            f"/recovery/{rehearsal_name}",
        )
        docker(
            "exec",
            args.target_container,
            "./openclaw",
            "memory",
            "maintenance",
            "status",
            "--database",
            "/data/openclaw-agent.sqlite",
        )
    except (Exception, KeyboardInterrupt) as error:
        print(f"Cutover failed: {error}", file=sys.stderr)
        if previous_stopped and data_retired:
            restore_after_failed_cutover(
                args.previous_container,
                args.target_container,
                retired_data,
                cutover_id,
            )
        elif previous_stopped:
            docker("start", args.previous_container)
        raise

    print(
        "Docker cutover completed.\n"
        f"Active Go container: {args.target_container}\n"
        f"Image: {image}\n"
        f"Gateway: {health_url(args.host_port)}\n"
        f"Retained previous container (stopped): {args.previous_container}\n"
        f"Retired pre-ledger data: {retired_data}\n"
        f"Fresh SQLite backup: {database_backup}\n"
        f"Restore rehearsal: {rehearsal}\n"
        "Live Telegram owner/non-owner acceptance remains a manual delivery-boundary gate.\n"
        "Rollback command:\n"
        f"  scripts/deploy-go-docker.py rollback --yes --previous-container {args.previous_container} "
        f"--target-container {args.target_container} --retired-data {retired_data}"
    )


def validated_retired_data(value: str) -> Path:
    candidate = Path(value).resolve()
    backup_root = BACKUP_DIR.resolve()
    if candidate.parent != backup_root:
        raise DeployError(f"retired data must be one direct child of {BACKUP_DIR}")
    require_directory(candidate, "retired data directory")
    return candidate


def rollback(args: argparse.Namespace) -> None:
    require_commands()
    retired_data = validated_retired_data(args.retired_data)
    if not container_exists(args.target_container):
        raise DeployError(f"active Go container does not exist: {args.target_container}")
    if not container_exists(args.previous_container):
        raise DeployError(f"retained previous container does not exist: {args.previous_container}")
    if container_running(args.previous_container):
        raise DeployError(f"retained previous container is already running: {args.previous_container}")
    require_directory(DATA_DIR, "active Go data directory")
    rollback_id = unique_cutover_id()
    rolled_back_data = BACKUP_DIR / f"agent_data_go.rolled-back-{rollback_id}"

    docker("stop", args.target_container)
    DATA_DIR.rename(rolled_back_data)
    try:
        retired_data.rename(DATA_DIR)
        docker("start", args.previous_container)
        wait_for_container(args.previous_container, True)
    except (Exception, KeyboardInterrupt) as original_error:
        try:
            if container_running(args.previous_container):
                docker("stop", args.previous_container)
            if DATA_DIR.exists() and not retired_data.exists():
                DATA_DIR.rename(retired_data)
            if rolled_back_data.exists() and not DATA_DIR.exists():
                rolled_back_data.rename(DATA_DIR)
            docker("start", args.target_container)
            wait_for_container(args.target_container, True)
        except (OSError, DeployError) as recovery_error:
            raise DeployError(
                f"rollback failed ({original_error}) and the Go runtime could not be restored: {recovery_error}"
            ) from original_error
        raise
    else:
        docker("rm", args.target_container)
    print(
        "Docker rollback completed.\n"
        f"Restored container: {args.previous_container}\n"
        f"Rolled-back Go data retained at: {rolled_back_data}"
    )


def main(arguments: list[str] | None = None) -> int:
    try:
        args = parse_args(arguments)
        if args.operation == "cutover":
            cutover(args)
        else:
            rollback(args)
    except KeyboardInterrupt:
        print("error: interrupted", file=sys.stderr)
        return 130
    except (OSError, DeployError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

#!/usr/bin/env python3

import importlib.util
import io
import sqlite3
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from unittest import mock


SCRIPT = Path(__file__).with_name("reset-go-test-state.py")
SPEC = importlib.util.spec_from_file_location("reset_go_test_state", SCRIPT)
reset = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = reset
SPEC.loader.exec_module(reset)


def create_database(path: Path, table: str = "marker") -> None:
    with sqlite3.connect(path) as connection:
        connection.execute(f"CREATE TABLE {table}(value TEXT)")


class ResetGoTestStateTests(unittest.TestCase):
    def test_build_creates_native_executable_candidate(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            go_dir = root / "golang"
            go_dir.mkdir()
            destination = root / "openclaw"

            def fake_run(arguments, **kwargs):
                self.assertEqual(
                    arguments[:4], ["go", "build", "-tags", "sqlite_fts5"]
                )
                self.assertEqual(kwargs["cwd"], go_dir)
                destination.write_bytes(b"candidate")
                destination.chmod(0o755)
                return ""

            with (
                mock.patch.object(reset, "GO_DIR", go_dir),
                mock.patch.object(reset, "run_command", side_effect=fake_run) as run,
            ):
                with redirect_stdout(io.StringIO()):
                    reset.build_candidate_binary(destination)

            run.assert_called_once()
            self.assertTrue(destination.is_file())
            self.assertNotEqual(destination.stat().st_mode & 0o111, 0)

    def test_backup_is_complete_and_does_not_modify_source(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "state.sqlite"
            backup = root / "backup.sqlite"
            with sqlite3.connect(source) as connection:
                connection.execute("CREATE TABLE marker(value TEXT)")
                connection.execute("INSERT INTO marker VALUES ('kept')")

            reset.backup_database(source, backup)

            with sqlite3.connect(source) as connection:
                self.assertEqual(
                    connection.execute("SELECT * FROM marker").fetchall(),
                    [("kept",)],
                )
            with sqlite3.connect(backup) as connection:
                self.assertEqual(
                    connection.execute("SELECT * FROM marker").fetchall(),
                    [("kept",)],
                )

    def test_service_configuration_requires_repository_paths(self):
        details = "\n".join(
            (
                f"WorkingDirectory={reset.ROOT_DIR}",
                f"ExecStart={{ path={reset.BINARY_FILE} ; argv[]={reset.BINARY_FILE} ; }}",
                "Environment="
                f"OPENCLAW_DATA_DIR={reset.DATA_DIR} "
                f"OPENCLAW_CONFIG_DIR={reset.CONFIG_DIR} PORT=18792",
            )
        )
        with (
            mock.patch.object(reset, "systemctl", return_value=details),
            mock.patch.object(reset, "service_active", return_value=True),
        ):
            reset.validate_service_configuration()

    def test_verification_runs_helper_and_complete_go_gates(self):
        calls = []

        def fake_run(arguments, **kwargs):
            calls.append((arguments, kwargs.get("cwd")))
            return ""

        with mock.patch.object(reset, "run_command", side_effect=fake_run):
            with redirect_stdout(io.StringIO()):
                reset.run_verification()

        self.assertEqual(
            calls,
            [
                (
                    [
                        "python3",
                        "-m",
                        "unittest",
                        "scripts/test_reset_go_test_state.py",
                    ],
                    reset.ROOT_DIR,
                ),
                (["go", "test", "-tags", "sqlite_fts5", "./..."], reset.GO_DIR),
                (
                    ["go", "test", "-tags", "sqlite_fts5", "-race", "./..."],
                    reset.GO_DIR,
                ),
                (["go", "vet", "-tags", "sqlite_fts5", "./..."], reset.GO_DIR),
                (["gofmt", "-l", "."], reset.GO_DIR),
                (["git", "diff", "--check"], reset.ROOT_DIR),
            ],
        )

    def test_backup_build_and_tests_precede_fresh_systemd_start(self):
        events = []
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config_dir = root / "config_test"
            data_dir = config_dir / "agent_data_go"
            backup_dir = config_dir / "sqlite_backups"
            go_dir = root / "golang"
            data_dir.mkdir(parents=True)
            go_dir.mkdir()
            (config_dir / "openclaw.json").write_text("{}")
            (config_dir / "secrets.json").write_text("{}")
            database = data_dir / "openclaw-agent.sqlite"
            binary = root / "openclaw"
            create_database(database)
            binary.write_bytes(b"previous")
            binary.chmod(0o755)

            original_backup = reset.backup_database

            def fake_backup(source, destination):
                events.append("backup")
                original_backup(source, destination)

            def fake_build(destination):
                events.append("build")
                destination.write_bytes(b"candidate")
                destination.chmod(0o755)

            def fake_install(candidate):
                events.append("install")
                binary.write_bytes(candidate.read_bytes())
                binary.chmod(0o755)

            def fake_systemctl(command, service, **_kwargs):
                self.assertEqual(service, reset.SERVICE)
                events.append(command)
                return ""

            def fake_wait():
                events.append("ready")
                if not database.exists():
                    create_database(database, "fresh")

            with (
                mock.patch.object(reset, "ROOT_DIR", root),
                mock.patch.object(reset, "GO_DIR", go_dir),
                mock.patch.object(reset, "CONFIG_DIR", config_dir),
                mock.patch.object(reset, "DATA_DIR", data_dir),
                mock.patch.object(reset, "BACKUP_DIR", backup_dir),
                mock.patch.object(reset, "DB_FILE", database),
                mock.patch.object(reset, "BINARY_FILE", binary),
                mock.patch.object(reset, "require_commands"),
                mock.patch.object(reset, "validate_service_configuration"),
                mock.patch.object(reset, "systemctl", side_effect=fake_systemctl),
                mock.patch.object(reset, "backup_database", side_effect=fake_backup),
                mock.patch.object(
                    reset, "build_candidate_binary", side_effect=fake_build
                ),
                mock.patch.object(
                    reset,
                    "run_verification",
                    side_effect=lambda: events.append("tests"),
                ),
                mock.patch.object(
                    reset, "install_candidate_binary", side_effect=fake_install
                ),
                mock.patch.object(
                    reset, "wait_for_fresh_runtime", side_effect=fake_wait
                ),
                mock.patch.object(
                    reset,
                    "assert_fresh_database",
                    side_effect=lambda _path: events.append("empty"),
                ),
            ):
                with redirect_stdout(io.StringIO()):
                    reset.reset_state()

            self.assertEqual(
                events,
                [
                    "stop",
                    "backup",
                    "build",
                    "tests",
                    "install",
                    "start",
                    "ready",
                    "empty",
                ],
            )
            backups = list(
                backup_dir.glob("openclaw-agent.sqlite.before-reset-*")
            )
            self.assertEqual(len(backups), 1)
            with sqlite3.connect(backups[0]) as connection:
                self.assertEqual(
                    connection.execute(
                        "SELECT name FROM sqlite_master WHERE name='marker'"
                    ).fetchone(),
                    ("marker",),
                )
            with sqlite3.connect(database) as connection:
                self.assertEqual(
                    connection.execute(
                        "SELECT name FROM sqlite_master WHERE name='fresh'"
                    ).fetchone(),
                    ("fresh",),
                )
            self.assertEqual(binary.read_bytes(), b"candidate")

    def test_build_failure_preserves_backup_and_restarts_old_runtime(self):
        events = []
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config_dir = root / "config_test"
            data_dir = config_dir / "agent_data_go"
            backup_dir = config_dir / "sqlite_backups"
            data_dir.mkdir(parents=True)
            (config_dir / "openclaw.json").write_text("{}")
            (config_dir / "secrets.json").write_text("{}")
            database = data_dir / "openclaw-agent.sqlite"
            binary = root / "openclaw"
            create_database(database)
            binary.write_bytes(b"previous")
            binary.chmod(0o755)

            def fake_systemctl(command, _service, **_kwargs):
                events.append(command)
                return ""

            with (
                mock.patch.object(reset, "ROOT_DIR", root),
                mock.patch.object(reset, "CONFIG_DIR", config_dir),
                mock.patch.object(reset, "DATA_DIR", data_dir),
                mock.patch.object(reset, "BACKUP_DIR", backup_dir),
                mock.patch.object(reset, "DB_FILE", database),
                mock.patch.object(reset, "BINARY_FILE", binary),
                mock.patch.object(reset, "require_commands"),
                mock.patch.object(reset, "validate_service_configuration"),
                mock.patch.object(reset, "systemctl", side_effect=fake_systemctl),
                mock.patch.object(
                    reset,
                    "build_candidate_binary",
                    side_effect=reset.ResetError("build failed"),
                ),
                mock.patch.object(reset, "service_active", return_value=False),
                mock.patch.object(reset, "wait_for_fresh_runtime"),
            ):
                with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                    with self.assertRaisesRegex(reset.ResetError, "build failed"):
                        reset.reset_state()

            self.assertEqual(events, ["stop", "start"])
            self.assertTrue(database.is_file())
            self.assertEqual(
                len(list(backup_dir.glob("openclaw-agent.sqlite.before-reset-*"))),
                1,
            )
            self.assertEqual(binary.read_bytes(), b"previous")

    def test_failed_fresh_start_restores_previous_database_and_binary(self):
        events = []
        waits = 0
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config_dir = root / "config_test"
            data_dir = config_dir / "agent_data_go"
            backup_dir = config_dir / "sqlite_backups"
            data_dir.mkdir(parents=True)
            (config_dir / "openclaw.json").write_text("{}")
            (config_dir / "secrets.json").write_text("{}")
            database = data_dir / "openclaw-agent.sqlite"
            binary = root / "openclaw"
            create_database(database)
            binary.write_bytes(b"previous")
            binary.chmod(0o755)

            def fake_build(destination):
                destination.write_bytes(b"candidate")
                destination.chmod(0o755)

            def fake_install(candidate):
                content = candidate.read_bytes()
                events.append(f"install-{content.decode()}")
                binary.write_bytes(content)
                binary.chmod(0o755)

            def fake_systemctl(command, _service, **_kwargs):
                events.append(command)
                return ""

            def fake_wait():
                nonlocal waits
                waits += 1
                if waits == 1:
                    create_database(database, "failed_fresh")
                    raise reset.ResetError("fresh startup failed")
                with sqlite3.connect(database) as connection:
                    self.assertEqual(
                        connection.execute(
                            "SELECT name FROM sqlite_master WHERE name='marker'"
                        ).fetchone(),
                        ("marker",),
                    )
                events.append("old-ready")

            with (
                mock.patch.object(reset, "ROOT_DIR", root),
                mock.patch.object(reset, "CONFIG_DIR", config_dir),
                mock.patch.object(reset, "DATA_DIR", data_dir),
                mock.patch.object(reset, "BACKUP_DIR", backup_dir),
                mock.patch.object(reset, "DB_FILE", database),
                mock.patch.object(reset, "BINARY_FILE", binary),
                mock.patch.object(reset, "require_commands"),
                mock.patch.object(reset, "validate_service_configuration"),
                mock.patch.object(reset, "systemctl", side_effect=fake_systemctl),
                mock.patch.object(
                    reset, "build_candidate_binary", side_effect=fake_build
                ),
                mock.patch.object(reset, "run_verification"),
                mock.patch.object(
                    reset, "install_candidate_binary", side_effect=fake_install
                ),
                mock.patch.object(
                    reset, "wait_for_fresh_runtime", side_effect=fake_wait
                ),
                mock.patch.object(reset, "service_active", return_value=False),
            ):
                with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                    with self.assertRaisesRegex(
                        reset.ResetError, "fresh startup failed"
                    ):
                        reset.reset_state()

            self.assertEqual(
                events,
                [
                    "stop",
                    "install-candidate",
                    "start",
                    "stop",
                    "install-previous",
                    "start",
                    "old-ready",
                ],
            )
            self.assertEqual(binary.read_bytes(), b"previous")
            with sqlite3.connect(database) as connection:
                self.assertEqual(
                    connection.execute(
                        "SELECT name FROM sqlite_master WHERE name='marker'"
                    ).fetchone(),
                    ("marker",),
                )
            failed = list(backup_dir.glob("failed-reset-*/openclaw-agent.sqlite"))
            self.assertEqual(len(failed), 1)


if __name__ == "__main__":
    unittest.main()

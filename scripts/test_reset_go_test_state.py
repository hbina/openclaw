#!/usr/bin/env python3

import importlib.util
import sqlite3
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


SCRIPT = Path(__file__).with_name("reset-go-test-state.py")
SPEC = importlib.util.spec_from_file_location("reset_go_test_state", SCRIPT)
reset = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = reset
SPEC.loader.exec_module(reset)


class ResetGoTestStateTests(unittest.TestCase):
    def test_build_extracts_executable_candidate_and_removes_helper_container(self):
        calls = []
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            go_dir = root / "golang"
            go_dir.mkdir()
            destination = root / "openclaw"

            def fake_docker(*arguments):
                calls.append(arguments)
                if arguments[0] == "create":
                    return "a" * 64
                if arguments[0] == "cp":
                    destination.write_bytes(b"candidate")
                    destination.chmod(0o755)
                return ""

            with (
                mock.patch.object(reset, "GO_DIR", go_dir),
                mock.patch.object(reset, "docker_stream") as build,
                mock.patch.object(reset, "docker", side_effect=fake_docker),
            ):
                reset.build_candidate_binary(destination)

        build.assert_called_once_with(
            "build", "--tag", reset.CANDIDATE_IMAGE, str(go_dir)
        )
        self.assertEqual(
            calls,
            [
                ("create", reset.CANDIDATE_IMAGE),
                ("cp", f"{'a' * 64}:{reset.CONTAINER_BINARY}", str(destination)),
                ("rm", "a" * 64),
            ],
        )

    def test_build_failure_happens_before_container_stop_or_state_archival(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            data_dir = root / "data"
            backup_dir = root / "backups"
            data_dir.mkdir()
            database = data_dir / "openclaw-agent.sqlite"
            with sqlite3.connect(database) as connection:
                connection.execute("CREATE TABLE marker(value TEXT)")

            docker_calls = []

            def fake_docker(*arguments):
                docker_calls.append(arguments)
                return "b" * 64

            with (
                mock.patch.object(reset, "DATA_DIR", data_dir),
                mock.patch.object(reset, "BACKUP_DIR", backup_dir),
                mock.patch.object(reset, "DB_FILE", database),
                mock.patch.object(reset.shutil, "which", return_value="/usr/bin/docker"),
                mock.patch.object(reset, "docker", side_effect=fake_docker),
                mock.patch.object(reset, "container_running", return_value=True),
                mock.patch.object(reset, "mounted_data_source", return_value=data_dir),
                mock.patch.object(
                    reset,
                    "build_candidate_binary",
                    side_effect=reset.ResetError("build failed"),
                ),
            ):
                with self.assertRaisesRegex(reset.ResetError, "build failed"):
                    reset.reset_state()

            self.assertTrue(database.is_file())
            self.assertFalse(backup_dir.exists() and any(backup_dir.iterdir()))
            self.assertFalse(any(call[0] == "stop" for call in docker_calls))

    def test_container_readiness_rejects_docker_restart_state(self):
        with mock.patch.object(
            reset,
            "docker",
            return_value="true true",
        ) as docker:
            self.assertFalse(reset.container_ready())
        docker.assert_called_once_with(
            "inspect",
            reset.CONTAINER,
            "--format",
            "{{.State.Running}} {{.State.Restarting}}",
        )

        with mock.patch.object(
            reset,
            "docker",
            side_effect=["true false", "OK"],
        ):
            self.assertTrue(reset.container_ready())

    def test_reset_installs_candidate_before_starting_fresh_database(self):
        events = []
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            data_dir = root / "data"
            backup_dir = root / "backups"
            data_dir.mkdir()
            database = data_dir / "openclaw-agent.sqlite"
            with sqlite3.connect(database) as connection:
                connection.execute("CREATE TABLE marker(value TEXT)")

            def fake_build(destination):
                events.append("build")
                destination.write_bytes(b"candidate")
                destination.chmod(0o755)

            def fake_install(candidate, verification):
                events.append("install")
                verification.write_bytes(candidate.read_bytes())

            def fake_docker(*arguments):
                if arguments[0] == "stop":
                    events.append("stop")
                elif arguments[0] == "start":
                    events.append("start")
                elif arguments[0] == "cp" and arguments[1].startswith(
                    f"{reset.CONTAINER}:"
                ):
                    previous = Path(arguments[2])
                    previous.write_bytes(b"previous")
                    previous.chmod(0o755)
                return "c" * 64

            def fake_wait():
                events.append("ready")
                with sqlite3.connect(database) as connection:
                    connection.execute("CREATE TABLE fresh(value TEXT)")

            with (
                mock.patch.object(reset, "DATA_DIR", data_dir),
                mock.patch.object(reset, "BACKUP_DIR", backup_dir),
                mock.patch.object(reset, "DB_FILE", database),
                mock.patch.object(reset.shutil, "which", return_value="/usr/bin/docker"),
                mock.patch.object(reset, "docker", side_effect=fake_docker),
                mock.patch.object(reset, "container_running", return_value=True),
                mock.patch.object(reset, "mounted_data_source", return_value=data_dir),
                mock.patch.object(reset, "build_candidate_binary", side_effect=fake_build),
                mock.patch.object(reset, "install_candidate_binary", side_effect=fake_install),
                mock.patch.object(reset, "wait_for_fresh_runtime", side_effect=fake_wait),
            ):
                reset.reset_state()

            self.assertEqual(events, ["build", "stop", "install", "start", "ready"])
            backups = list(backup_dir.glob("openclaw-agent.sqlite.before-reset-*"))
            self.assertEqual(len(backups), 1)
            with sqlite3.connect(backups[0]) as connection:
                self.assertEqual(
                    connection.execute(
                        "SELECT name FROM sqlite_master WHERE name='marker'"
                    ).fetchone(),
                    ("marker",),
                )


if __name__ == "__main__":
    unittest.main()

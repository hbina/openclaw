#!/usr/bin/env python3

import importlib.util
import io
import sqlite3
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from types import SimpleNamespace
from unittest import mock


SCRIPT = Path(__file__).with_name("deploy-go-docker.py")
SPEC = importlib.util.spec_from_file_location("deploy_go_docker", SCRIPT)
deploy = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = deploy
SPEC.loader.exec_module(deploy)


class DeployGoDockerTests(unittest.TestCase):
    def test_cutover_requires_confirmation_and_distinct_container_names(self):
        with redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                deploy.parse_args(["cutover", "--previous-container", "old"])
            with self.assertRaises(SystemExit):
                deploy.parse_args(
                    [
                        "cutover",
                        "--yes",
                        "--previous-container",
                        "same",
                        "--target-container",
                        "same",
                    ]
                )

    def test_backup_is_complete_and_source_is_unchanged(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source.sqlite"
            backup = root / "backup.sqlite"
            with sqlite3.connect(source) as database:
                database.execute("CREATE TABLE marker(value TEXT)")
                database.execute("INSERT INTO marker VALUES ('kept')")

            deploy.backup_database(source, backup)

            for path in (source, backup):
                with sqlite3.connect(path) as database:
                    self.assertEqual(
                        database.execute("SELECT * FROM marker").fetchall(),
                        [("kept",)],
                    )

    def test_cutover_configuration_requires_owner_maintenance_and_bot_token(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            public = root / "openclaw.json"
            secrets = root / "secrets.json"
            public.write_text(
                '{"channels":{"telegram":{"enabled":true,"ownerUserId":"100"}},'
                '"agents":{"defaults":{"memoryMaintenance":{"enabled":true}}}}'
            )
            secrets.write_text(
                '{"channels":{"telegram":{"botToken":"not-a-real-token"}}}'
            )
            with (
                mock.patch.object(deploy, "CONFIG_FILE", public),
                mock.patch.object(deploy, "SECRETS_FILE", secrets),
            ):
                deploy.validate_cutover_configuration()
                secrets.write_text("{}")
                with self.assertRaisesRegex(deploy.DeployError, "bot token"):
                    deploy.validate_cutover_configuration()

    def test_fresh_database_check_includes_maintenance_candidates(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "state.sqlite"
            tables = (
                "memories",
                "memory_revisions",
                "memory_candidates",
                "conversation_history",
                "tasks",
                "reminders",
                "response_traces",
            )
            with sqlite3.connect(path) as database:
                for table in tables:
                    database.execute(f"CREATE TABLE {table}(value TEXT)")
                database.execute(
                    "CREATE TABLE memory_maintenance_state(singleton_id INTEGER, checkpoint_history_id INTEGER)"
                )
                database.execute("INSERT INTO memory_maintenance_state VALUES (1, 0)")
            deploy.assert_fresh_database(path)
            with sqlite3.connect(path) as database:
                database.execute("INSERT INTO memory_candidates VALUES ('candidate')")
            with self.assertRaisesRegex(deploy.DeployError, "memory_candidates"):
                deploy.assert_fresh_database(path)

    def test_smoke_persistence_requires_trace_exchange_chunk_and_no_mutation(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "state.sqlite"
            with sqlite3.connect(path) as database:
                database.execute(
                    "CREATE TABLE response_traces(channel_id TEXT, sender_id TEXT, status TEXT)"
                )
                database.execute(
                    "CREATE TABLE conversation_history(channel_id TEXT, sender_id TEXT)"
                )
                database.execute("CREATE TABLE conversation_chunks(value TEXT)")
                for table in ("memories", "tasks", "reminders"):
                    database.execute(f"CREATE TABLE {table}(value TEXT)")
                database.execute(
                    "INSERT INTO response_traces VALUES ('cli', 'proof', 'completed')"
                )
                database.executemany(
                    "INSERT INTO conversation_history VALUES ('cli', 'proof')",
                    [(), ()],
                )
                database.execute("INSERT INTO conversation_chunks VALUES ('indexed')")
            deploy.assert_smoke_persisted(path, "proof")
            with sqlite3.connect(path) as database:
                database.execute("INSERT INTO tasks VALUES ('unexpected')")
            with self.assertRaisesRegex(deploy.DeployError, "unexpectedly mutated"):
                deploy.assert_smoke_persisted(path, "proof")

    def test_retired_data_must_be_direct_backup_child(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            backup = root / "backups"
            valid = backup / "agent_data_go.before-test"
            invalid = root / "outside"
            valid.mkdir(parents=True)
            invalid.mkdir()
            with mock.patch.object(deploy, "BACKUP_DIR", backup):
                self.assertEqual(deploy.validated_retired_data(str(valid)), valid)
                with self.assertRaisesRegex(deploy.DeployError, "direct child"):
                    deploy.validated_retired_data(str(invalid))

    def test_verification_runs_complete_go_gates(self):
        calls = []

        def fake_run(arguments, **kwargs):
            calls.append((arguments, kwargs.get("cwd")))
            return ""

        with mock.patch.object(deploy, "run_command", side_effect=fake_run):
            deploy.run_verification()

        self.assertEqual(
            calls,
            [
                (
                    [
                        "python3",
                        "-m",
                        "unittest",
                        "scripts/test_deploy_go_docker.py",
                        "scripts/test_reset_go_test_state.py",
                    ],
                    deploy.ROOT_DIR,
                ),
                (["go", "test", "-tags", "sqlite_fts5", "./..."], deploy.GO_DIR),
                (["go", "test", "-race", "-tags", "sqlite_fts5", "./..."], deploy.GO_DIR),
                (["go", "vet", "-tags", "sqlite_fts5", "./..."], deploy.GO_DIR),
                (["gofmt", "-l", "."], deploy.GO_DIR),
                (["git", "diff", "--check"], deploy.ROOT_DIR),
            ],
        )

    def test_cutover_archives_old_data_and_proves_backup_rehearsal(self):
        events = []
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config_dir = root / "config"
            data_dir = config_dir / "agent_data_go"
            backup_dir = config_dir / "backups"
            config_dir.mkdir()
            data_dir.mkdir()
            config_file = config_dir / "openclaw.json"
            secrets_file = config_dir / "secrets.json"
            database_file = data_dir / "openclaw-agent.sqlite"
            config_file.write_text("{}")
            secrets_file.write_text("{}")
            (data_dir / "old-state").write_text("preserved")

            def create_fresh_database():
                tables = (
                    "memories",
                    "memory_revisions",
                    "memory_candidates",
                    "conversation_history",
                    "tasks",
                    "reminders",
                    "response_traces",
                )
                with sqlite3.connect(database_file) as database:
                    for table in tables:
                        database.execute(f"CREATE TABLE {table}(value TEXT)")
                    database.execute(
                        "CREATE TABLE memory_maintenance_state(singleton_id INTEGER, checkpoint_history_id INTEGER)"
                    )
                    database.execute("INSERT INTO memory_maintenance_state VALUES (1, 0)")

            def fake_start(_image, _target, _port):
                events.append("start-candidate")
                create_fresh_database()

            def fake_docker(*arguments, **_kwargs):
                events.append("docker-" + arguments[0])
                return ""

            args = SimpleNamespace(
                previous_container="node-old",
                target_container="go-new",
                host_port=18792,
                tag="test-tag",
            )
            with (
                mock.patch.object(deploy, "CONFIG_DIR", config_dir),
                mock.patch.object(deploy, "CONFIG_FILE", config_file),
                mock.patch.object(deploy, "SECRETS_FILE", secrets_file),
                mock.patch.object(deploy, "DATA_DIR", data_dir),
                mock.patch.object(deploy, "BACKUP_DIR", backup_dir),
                mock.patch.object(deploy, "DB_FILE", database_file),
                mock.patch.object(deploy, "validate_common", side_effect=lambda *_: events.append("validate")),
                mock.patch.object(deploy, "unique_cutover_id", return_value="fixed"),
                mock.patch.object(deploy, "image_exists", return_value=False),
                mock.patch.object(deploy, "run_verification", side_effect=lambda: events.append("verify")),
                mock.patch.object(deploy, "build_image", side_effect=lambda _image: events.append("build")),
                mock.patch.object(deploy, "docker", side_effect=fake_docker),
                mock.patch.object(deploy, "wait_for_container"),
                mock.patch.object(deploy, "start_candidate", side_effect=fake_start),
                mock.patch.object(deploy, "wait_for_gateway", side_effect=lambda _port: events.append("healthy")),
                mock.patch.object(deploy, "post_smoke_chat", side_effect=lambda *_: events.append("smoke")),
                mock.patch.object(deploy, "assert_smoke_persisted"),
            ):
                with redirect_stdout(io.StringIO()):
                    deploy.cutover(args)

            retired = backup_dir / "agent_data_go.before-fixed"
            self.assertEqual((retired / "old-state").read_text(), "preserved")
            self.assertTrue(database_file.is_file())
            self.assertTrue((backup_dir / "openclaw-agent.after-fixed.sqlite").is_file())
            self.assertTrue((backup_dir / "openclaw-agent.rehearsal-fixed.sqlite").is_file())
            self.assertEqual(events[:4], ["validate", "verify", "build", "docker-stop"])
            self.assertIn("start-candidate", events)
            self.assertIn("smoke", events)
            self.assertIn("docker-restart", events)
            self.assertGreaterEqual(events.count("docker-run"), 1)

    def test_failed_cutover_restores_old_data_and_previous_container(self):
        events = []
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            backup_dir = root / "backups"
            data_dir = root / "agent_data_go"
            retired = backup_dir / "agent_data_go.before-test"
            backup_dir.mkdir()
            data_dir.mkdir()
            retired.mkdir()
            (data_dir / "fresh").write_text("failed")
            (retired / "old").write_text("restored")

            def fake_docker(*arguments, **_kwargs):
                events.append(arguments[:2])
                return ""

            with (
                mock.patch.object(deploy, "BACKUP_DIR", backup_dir),
                mock.patch.object(deploy, "DATA_DIR", data_dir),
                mock.patch.object(deploy, "container_exists", return_value=True),
                mock.patch.object(deploy, "docker", side_effect=fake_docker),
                mock.patch.object(deploy, "wait_for_container"),
            ):
                with redirect_stderr(io.StringIO()):
                    deploy.restore_after_failed_cutover(
                        "node-old", "go-new", retired, "test"
                    )

            self.assertEqual((data_dir / "old").read_text(), "restored")
            self.assertEqual(
                (backup_dir / "agent_data_go.failed-test" / "fresh").read_text(),
                "failed",
            )
            self.assertEqual(events, [("rm", "-f"), ("start", "node-old")])


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
from __future__ import annotations

import argparse
import datetime as dt
import http.client
import json
import os
import pathlib
import platform
import re
import shutil
import signal
import socket
import statistics
import subprocess
import sys
import time
from dataclasses import asdict, dataclass
from typing import Any


DEFAULT_SERVER_BIN = "/usr/local/bin/llama-server"
DEFAULT_PERF_BIN = "/home/hbina085/workspace/linux/tools/perf/perf"
DEFAULT_MODEL = "model/gemma-4-26b-a4b-it-Q4_K_M.gguf"
DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 8080
DEFAULT_THREADS = 16
DEFAULT_CTX_SIZE = 100000
DEFAULT_NGL = 0
DEFAULT_MAX_TOKENS = 64
DEFAULT_RUNS = 5
DEFAULT_WARMUP = 1
DEFAULT_STARTUP_TIMEOUT_SECONDS = 180.0
DEFAULT_REQUEST_TIMEOUT_SECONDS = 120.0
DEFAULT_PERF_FREQUENCY = 999
DEFAULT_PERF_EVENTS = ",".join(
    [
        "cycles",
        "instructions",
        "branches",
        "branch-misses",
        "cache-misses",
        "context-switches",
        "cpu-migrations",
        "page-faults",
    ]
)
DEFAULT_PROMPT = "Reply with exactly: benchmark-ok"
REPO_ROOT = pathlib.Path.cwd()


class HarnessError(RuntimeError):
    pass


@dataclass
class TimingSummary:
    avg: float
    p50: float
    p95: float
    min: float
    max: float


@dataclass
class RunResult:
    run_index: int
    response_path: str
    perf_stat_path: str | None
    perf_record_path: str | None
    perf_report_path: str | None
    started_at: str
    ttfb_ms: float | None
    ttft_ms: float | None
    total_latency_ms: float
    completion_text: str
    completion_chars: int
    completion_tokens: int | None
    prompt_tokens: int | None
    total_tokens: int | None
    decode_tokens_per_second: float | None
    perf_stat_metrics: dict[str, float | int | str]
    usage: dict[str, Any] | None
    finish_reason: str | None


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Benchmark a local llama-server instance with perf stat and perf record."
    )
    parser.add_argument("--server-bin", default=DEFAULT_SERVER_BIN)
    parser.add_argument("--perf-bin", default=DEFAULT_PERF_BIN)
    parser.add_argument("--host", default=DEFAULT_HOST)
    parser.add_argument("--port", type=int, default=DEFAULT_PORT)
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument("--threads", type=int, default=DEFAULT_THREADS)
    parser.add_argument("--ctx-size", type=int, default=DEFAULT_CTX_SIZE)
    parser.add_argument("--ngl", type=int, default=DEFAULT_NGL)
    parser.add_argument("--prompt", default=DEFAULT_PROMPT)
    parser.add_argument("--max-tokens", type=int, default=DEFAULT_MAX_TOKENS)
    parser.add_argument("--runs", type=int, default=DEFAULT_RUNS)
    parser.add_argument("--warmup", type=int, default=DEFAULT_WARMUP)
    parser.add_argument("--perf-frequency", type=int, default=DEFAULT_PERF_FREQUENCY)
    parser.add_argument("--perf-events", default=DEFAULT_PERF_EVENTS)
    parser.add_argument("--startup-timeout-seconds", type=float, default=DEFAULT_STARTUP_TIMEOUT_SECONDS)
    parser.add_argument("--request-timeout-seconds", type=float, default=DEFAULT_REQUEST_TIMEOUT_SECONDS)
    parser.add_argument("--output-dir", default=None)
    parser.add_argument("--server-arg", action="append", default=[])
    parser.add_argument("--no-perf-stat", action="store_true")
    parser.add_argument("--no-perf-record", action="store_true")
    parser.add_argument("--no-stop-existing", action="store_true")
    parser.add_argument("--temperature", type=float, default=0.0)
    parser.add_argument("--top-p", type=float, default=1.0)
    return parser.parse_args(argv)


def utc_timestamp() -> str:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat()


def timestamp_slug() -> str:
    return dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")


def format_command(command: list[str]) -> str:
    return " ".join(shlex_quote(part) for part in command)


def shlex_quote(value: str) -> str:
    if not value:
        return "''"
    if all(ch.isalnum() or ch in "@%_+=:,./-" for ch in value):
        return value
    return "'" + value.replace("'", "'\"'\"'") + "'"


def summarize(values: list[float]) -> TimingSummary | None:
    if not values:
        return None
    ordered = sorted(values)
    return TimingSummary(
        avg=statistics.fmean(ordered),
        p50=statistics.median(ordered),
        p95=ordered[min(len(ordered) - 1, int(len(ordered) * 0.95))],
        min=ordered[0],
        max=ordered[-1],
    )


def require_binary(path_or_name: str) -> str:
    candidate = pathlib.Path(path_or_name)
    if candidate.is_absolute() or "/" in path_or_name:
        if candidate.exists() and os.access(candidate, os.X_OK):
            return str(candidate)
        raise HarnessError(f"Required binary is not executable: {path_or_name}")
    resolved = shutil.which(path_or_name)
    if resolved:
        return resolved
    raise HarnessError(f"Required binary not found on PATH: {path_or_name}")


def run_capture(command: list[str], *, timeout: float | None = None) -> str:
    result = subprocess.run(
        command,
        capture_output=True,
        text=True,
        timeout=timeout,
        check=False,
        env={**os.environ, "PAGER": "cat"},
    )
    if result.returncode != 0:
        stderr = (result.stderr or result.stdout).strip()
        raise HarnessError(f"Command failed ({format_command(command)}): {stderr}")
    return result.stdout.strip()


def port_in_use(host: str, port: int) -> bool:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.settimeout(0.2)
        return sock.connect_ex((host, port)) == 0


def parse_listener_from_ss_output(output: str, port: int) -> tuple[int, str] | None:
    for line in output.splitlines():
        if "LISTEN" not in line or f":{port}" not in line:
            continue
        pid_match = re.search(r"pid=(\d+)", line)
        name_match = re.search(r'users:\(\("([^"]+)"', line)
        if pid_match and name_match:
            return int(pid_match.group(1)), name_match.group(1)
    return None


def terminate_existing_listener(host: str, port: int) -> None:
    for _ in range(5):
        try:
            ss_output = run_capture(["ss", "-ltnp"], timeout=5)
        except HarnessError as exc:
            raise HarnessError(
                f"{host}:{port} is already in use and the harness could not inspect the listener: {exc}"
            ) from exc
        listener = parse_listener_from_ss_output(ss_output, port)
        if not listener:
            if not port_in_use(host, port):
                return
            raise HarnessError(
                f"{host}:{port} is already in use and the harness could not identify the listener safely."
            )
        pid, process_name = listener
        if process_name != "llama-server":
            raise HarnessError(
                f"{host}:{port} is already in use by {process_name} (pid {pid}); refusing to stop a non-llama-server process."
            )
        try:
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            if not port_in_use(host, port):
                return
            continue
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if not port_in_use(host, port):
                return
            time.sleep(0.2)
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            if not port_in_use(host, port):
                return
            continue
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if not port_in_use(host, port):
                return
            time.sleep(0.2)
    raise HarnessError(f"Failed to stop existing llama-server on {host}:{port}")


def read_mem_total_bytes() -> int | None:
    meminfo = pathlib.Path("/proc/meminfo")
    if not meminfo.exists():
        return None
    for line in meminfo.read_text(encoding="utf-8").splitlines():
        if line.startswith("MemTotal:"):
            parts = line.split()
            if len(parts) >= 2 and parts[1].isdigit():
                return int(parts[1]) * 1024
    return None


def collect_environment() -> dict[str, Any]:
    uname = platform.uname()
    return {
        "timestamp": utc_timestamp(),
        "hostname": socket.gethostname(),
        "platform": {
            "system": uname.system,
            "release": uname.release,
            "version": uname.version,
            "machine": uname.machine,
            "processor": uname.processor,
            "python": platform.python_version(),
            "cpu_count": os.cpu_count(),
            "mem_total_bytes": read_mem_total_bytes(),
        },
    }


def resolve_model_path(raw_model: str) -> str:
    model_path = pathlib.Path(raw_model)
    if model_path.is_absolute():
        return str(model_path)
    for candidate in [REPO_ROOT / model_path, REPO_ROOT.parent / model_path]:
        if candidate.exists():
            return str(candidate.resolve())
    return raw_model


def tail_text(path: pathlib.Path, line_count: int = 40) -> str:
    if not path.exists():
        return ""
    lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
    return "\n".join(lines[-line_count:])


def wait_for_health(
    host: str,
    port: int,
    timeout_seconds: float,
    *,
    process: subprocess.Popen[bytes] | None = None,
    stderr_path: pathlib.Path | None = None,
) -> None:
    deadline = time.monotonic() + timeout_seconds
    last_error: str | None = None
    while time.monotonic() < deadline:
        if process and process.poll() is not None:
            stderr_tail = tail_text(stderr_path) if stderr_path else ""
            detail = stderr_tail or f"exit code {process.returncode}"
            raise HarnessError(f"llama-server exited during startup: {detail}")
        try:
            conn = http.client.HTTPConnection(host, port, timeout=2)
            conn.request("GET", "/health")
            response = conn.getresponse()
            payload = response.read().decode("utf-8", errors="replace")
            conn.close()
            if response.status == 200:
                data = json.loads(payload)
                if data.get("status") == "ok":
                    return
                last_error = f"/health returned unexpected payload: {payload}"
            else:
                last_error = f"/health returned status {response.status}"
        except Exception as exc:  # noqa: BLE001
            last_error = str(exc)
        time.sleep(0.5)
    raise HarnessError(f"Timed out waiting for server health on {host}:{port}: {last_error or 'unknown error'}")


class ManagedServer:
    def __init__(self, args: argparse.Namespace, artifact_dir: pathlib.Path) -> None:
        self.args = args
        self.artifact_dir = artifact_dir
        self.process: subprocess.Popen[bytes] | None = None
        self.stdout_path = artifact_dir / "server.stdout.log"
        self.stderr_path = artifact_dir / "server.stderr.log"
        self.command = self._build_command()

    def _build_command(self) -> list[str]:
        return [
            self.args.server_bin,
            "-m",
            resolve_model_path(self.args.model),
            "-ngl",
            str(self.args.ngl),
            "-t",
            str(self.args.threads),
            "-c",
            str(self.args.ctx_size),
            "--host",
            self.args.host,
            "--port",
            str(self.args.port),
            *self.args.server_arg,
        ]

    def start(self) -> None:
        if port_in_use(self.args.host, self.args.port):
            if self.args.no_stop_existing:
                raise HarnessError(
                    f"{self.args.host}:{self.args.port} is already in use. Stop the existing service or omit --no-stop-existing."
                )
            terminate_existing_listener(self.args.host, self.args.port)
        stdout_handle = self.stdout_path.open("wb")
        stderr_handle = self.stderr_path.open("wb")
        try:
            self.process = subprocess.Popen(
                self.command,
                stdout=stdout_handle,
                stderr=stderr_handle,
                stdin=subprocess.DEVNULL,
                start_new_session=True,
            )
        except Exception:
            stdout_handle.close()
            stderr_handle.close()
            raise
        wait_for_health(
            self.args.host,
            self.args.port,
            self.args.startup_timeout_seconds,
            process=self.process,
            stderr_path=self.stderr_path,
        )

    @property
    def pid(self) -> int:
        if not self.process:
            raise HarnessError("llama-server process is not running")
        return self.process.pid

    def stop(self) -> None:
        if not self.process:
            return
        if self.process.poll() is not None:
            return
        try:
            os.killpg(self.process.pid, signal.SIGTERM)
        except ProcessLookupError:
            return
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                return
            time.sleep(0.2)
        try:
            os.killpg(self.process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass


class PerfSession:
    def __init__(
        self,
        perf_bin: str,
        pid: int,
        artifact_dir: pathlib.Path,
        run_index: int,
        perf_events: str,
        perf_frequency: int,
        enabled_stat: bool,
        enabled_record: bool,
    ) -> None:
        self.perf_bin = perf_bin
        self.pid = pid
        self.artifact_dir = artifact_dir
        self.run_index = run_index
        self.perf_events = perf_events
        self.perf_frequency = perf_frequency
        self.enabled_stat = enabled_stat
        self.enabled_record = enabled_record
        self.stat_path = artifact_dir / f"perf-stat.run-{run_index}.txt"
        self.data_path = artifact_dir / f"perf.data.run-{run_index}"
        self.report_path = artifact_dir / f"perf-report.run-{run_index}.txt"
        self.stat_proc: subprocess.Popen[bytes] | None = None
        self.record_proc: subprocess.Popen[bytes] | None = None

    def start(self) -> None:
        null_in = subprocess.DEVNULL
        if self.enabled_stat:
            stat_command = [
                self.perf_bin,
                "stat",
                "-x",
                ",",
                "-e",
                self.perf_events,
                "-p",
                str(self.pid),
                "-o",
                str(self.stat_path),
            ]
            self.stat_proc = subprocess.Popen(
                stat_command,
                stdin=null_in,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                start_new_session=True,
            )
        if self.enabled_record:
            record_command = [
                self.perf_bin,
                "record",
                "-F",
                str(self.perf_frequency),
                "-g",
                "-p",
                str(self.pid),
                "-o",
                str(self.data_path),
            ]
            self.record_proc = subprocess.Popen(
                record_command,
                stdin=null_in,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                start_new_session=True,
            )
        time.sleep(0.15)

    def stop(self) -> None:
        for proc in [self.record_proc, self.stat_proc]:
            if not proc or proc.poll() is not None:
                continue
            try:
                os.killpg(proc.pid, signal.SIGINT)
            except ProcessLookupError:
                continue
        for proc in [self.record_proc, self.stat_proc]:
            if not proc:
                continue
            try:
                proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
        if self.enabled_record and self.data_path.exists():
            report_command = [
                self.perf_bin,
                "report",
                "--stdio",
                "--sort",
                "comm,dso,symbol",
                "--percent-limit",
                "0.5",
                "-i",
                str(self.data_path),
            ]
            result = subprocess.run(
                report_command,
                capture_output=True,
                text=True,
                check=False,
                env={**os.environ, "PAGER": "cat"},
            )
            self.report_path.write_text(
                (result.stdout or "") + (result.stderr or ""),
                encoding="utf-8",
            )


def build_request_payload(args: argparse.Namespace) -> dict[str, Any]:
    return {
        "model": pathlib.Path(args.model).name,
        "messages": [{"role": "user", "content": args.prompt}],
        "temperature": args.temperature,
        "top_p": args.top_p,
        "max_tokens": args.max_tokens,
        "stream": True,
        "stream_options": {"include_usage": True},
    }


def stream_chat_completion(
    host: str,
    port: int,
    payload: dict[str, Any],
    timeout_seconds: float,
    response_path: pathlib.Path,
) -> dict[str, Any]:
    conn = http.client.HTTPConnection(host, port, timeout=timeout_seconds)
    body = json.dumps(payload).encode("utf-8")
    request_started = time.perf_counter()
    conn.request("POST", "/v1/chat/completions", body=body, headers={"Content-Type": "application/json"})
    response = conn.getresponse()
    first_byte_ms = (time.perf_counter() - request_started) * 1000.0
    if response.status != 200:
        payload_text = response.read().decode("utf-8", errors="replace")
        conn.close()
        raise HarnessError(f"Chat completion failed with status {response.status}: {payload_text}")

    first_token_ms: float | None = None
    completion_parts: list[str] = []
    usage: dict[str, Any] | None = None
    finish_reason: str | None = None
    content_chunks = 0

    with response_path.open("w", encoding="utf-8") as log_file:
        while True:
            raw_line = response.readline()
            if not raw_line:
                break
            decoded = raw_line.decode("utf-8", errors="replace")
            log_file.write(decoded)
            stripped = decoded.strip()
            if not stripped or not stripped.startswith("data: "):
                continue
            data = stripped[6:]
            if data == "[DONE]":
                break
            event = json.loads(data)
            if event.get("usage"):
                usage = event["usage"]
            choice = (event.get("choices") or [{}])[0]
            delta = choice.get("delta") or {}
            content = delta.get("content")
            if content:
                completion_parts.append(content)
                content_chunks += 1
                if first_token_ms is None:
                    first_token_ms = (time.perf_counter() - request_started) * 1000.0
            if choice.get("finish_reason"):
                finish_reason = choice["finish_reason"]

    total_latency_ms = (time.perf_counter() - request_started) * 1000.0
    conn.close()
    completion_text = "".join(completion_parts)
    completion_tokens = usage.get("completion_tokens") if isinstance(usage, dict) else None
    prompt_tokens = usage.get("prompt_tokens") if isinstance(usage, dict) else None
    total_tokens = usage.get("total_tokens") if isinstance(usage, dict) else None
    decode_tokens_per_second = None
    if completion_tokens and total_latency_ms > 0:
        decode_tokens_per_second = completion_tokens / (total_latency_ms / 1000.0)
    return {
        "ttfb_ms": first_byte_ms,
        "ttft_ms": first_token_ms,
        "total_latency_ms": total_latency_ms,
        "completion_text": completion_text,
        "completion_chars": len(completion_text),
        "completion_tokens": completion_tokens,
        "prompt_tokens": prompt_tokens,
        "total_tokens": total_tokens,
        "decode_tokens_per_second": decode_tokens_per_second,
        "usage": usage,
        "finish_reason": finish_reason,
        "content_chunks": content_chunks,
    }


def parse_perf_stat(path: pathlib.Path) -> dict[str, float | int | str]:
    if not path.exists():
        return {}
    metrics: dict[str, float | int | str] = {}
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        parts = [part.strip() for part in line.split(",")]
        if len(parts) < 3:
            continue
        raw_value, _, metric_name = parts[:3]
        if not metric_name or metric_name == "<not supported>":
            continue
        value = raw_value.replace(" ", "")
        if value in {"<notcounted>", "<not supported>", ""}:
            continue
        normalized = metric_name.replace(":u", "")
        try:
            if any(ch in value for ch in ".eE"):
                metrics[normalized] = float(value)
            else:
                metrics[normalized] = int(value)
        except ValueError:
            metrics[normalized] = value
    return metrics


def relative_to_repo(path: pathlib.Path) -> str:
    try:
        return str(path.relative_to(pathlib.Path.cwd()))
    except ValueError:
        return str(path)


def run_single_iteration(
    args: argparse.Namespace,
    perf_bin: str,
    pid: int,
    artifact_dir: pathlib.Path,
    request_payload: dict[str, Any],
    run_index: int,
    *,
    capture_perf: bool,
) -> RunResult:
    response_path = artifact_dir / f"response.run-{run_index}.ndjson"
    perf = PerfSession(
        perf_bin=perf_bin,
        pid=pid,
        artifact_dir=artifact_dir,
        run_index=run_index,
        perf_events=args.perf_events,
        perf_frequency=args.perf_frequency,
        enabled_stat=capture_perf and not args.no_perf_stat,
        enabled_record=capture_perf and not args.no_perf_record,
    )
    perf.start()
    try:
        request_result = stream_chat_completion(
            args.host,
            args.port,
            request_payload,
            args.request_timeout_seconds,
            response_path,
        )
    finally:
        perf.stop()
    perf_metrics = parse_perf_stat(perf.stat_path) if capture_perf and not args.no_perf_stat else {}
    return RunResult(
        run_index=run_index,
        response_path=relative_to_repo(response_path),
        perf_stat_path=relative_to_repo(perf.stat_path) if capture_perf and not args.no_perf_stat else None,
        perf_record_path=relative_to_repo(perf.data_path) if capture_perf and not args.no_perf_record else None,
        perf_report_path=relative_to_repo(perf.report_path) if capture_perf and not args.no_perf_record else None,
        started_at=utc_timestamp(),
        ttfb_ms=request_result["ttfb_ms"],
        ttft_ms=request_result["ttft_ms"],
        total_latency_ms=request_result["total_latency_ms"],
        completion_text=request_result["completion_text"],
        completion_chars=request_result["completion_chars"],
        completion_tokens=request_result["completion_tokens"],
        prompt_tokens=request_result["prompt_tokens"],
        total_tokens=request_result["total_tokens"],
        decode_tokens_per_second=request_result["decode_tokens_per_second"],
        perf_stat_metrics=perf_metrics,
        usage=request_result["usage"],
        finish_reason=request_result["finish_reason"],
    )


def summarize_runs(runs: list[RunResult]) -> dict[str, Any]:
    def pack(metric_values: list[float | None]) -> dict[str, float] | None:
        values = [float(value) for value in metric_values if value is not None]
        summary = summarize(values)
        return asdict(summary) if summary else None

    return {
        "ttfb_ms": pack([run.ttfb_ms for run in runs]),
        "ttft_ms": pack([run.ttft_ms for run in runs]),
        "total_latency_ms": pack([run.total_latency_ms for run in runs]),
        "decode_tokens_per_second": pack([run.decode_tokens_per_second for run in runs]),
    }


def resolve_output_dir(user_value: str | None) -> pathlib.Path:
    if user_value:
        return pathlib.Path(user_value)
    return pathlib.Path(".artifacts") / "llama-server-perf" / timestamp_slug()


def write_json(path: pathlib.Path, payload: dict[str, Any]) -> None:
    path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    if args.runs <= 0:
        raise HarnessError("--runs must be > 0")
    if args.warmup < 0:
        raise HarnessError("--warmup must be >= 0")

    args.server_bin = require_binary(args.server_bin)
    args.perf_bin = require_binary(args.perf_bin)
    output_dir = resolve_output_dir(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)

    request_payload = build_request_payload(args)
    write_json(output_dir / "request.json", request_payload)

    metadata = {
        "environment": collect_environment(),
        "tool_versions": {
            "llama_server": run_capture([args.server_bin, "--version"], timeout=10),
            "perf": run_capture([args.perf_bin, "--version"], timeout=10),
        },
        "server": {
            "binary": args.server_bin,
            "model": args.model,
            "host": args.host,
            "port": args.port,
            "threads": args.threads,
            "ctx_size": args.ctx_size,
            "ngl": args.ngl,
            "extra_args": args.server_arg,
        },
        "request": request_payload,
    }

    server = ManagedServer(args, output_dir)
    measured_runs: list[RunResult] = []

    def stop_server(*_: Any) -> None:
        server.stop()

    old_sigint = signal.signal(signal.SIGINT, stop_server)
    old_sigterm = signal.signal(signal.SIGTERM, stop_server)

    try:
        server.start()
        metadata["server"]["pid"] = server.pid
        metadata["server"]["command"] = server.command
        metadata["server"]["command_pretty"] = format_command(server.command)
        write_json(output_dir / "metadata.json", metadata)

        for warmup_index in range(1, args.warmup + 1):
            run_single_iteration(
                args,
                args.perf_bin,
                server.pid,
                output_dir,
                request_payload,
                run_index=warmup_index,
                capture_perf=False,
            )

        for run_index in range(1, args.runs + 1):
            result = run_single_iteration(
                args,
                args.perf_bin,
                server.pid,
                output_dir,
                request_payload,
                run_index=run_index,
                capture_perf=True,
            )
            measured_runs.append(result)
            ttfb_display = f"{result.ttfb_ms:.1f}" if result.ttfb_ms is not None else "n/a"
            ttft_display = f"{result.ttft_ms:.1f}" if result.ttft_ms is not None else "n/a"
            print(
                f"run {run_index}/{args.runs}: total={result.total_latency_ms:.1f}ms "
                f"ttfb={ttfb_display}ms "
                f"ttft={ttft_display}ms"
            )
    finally:
        server.stop()
        signal.signal(signal.SIGINT, old_sigint)
        signal.signal(signal.SIGTERM, old_sigterm)

    summary = {
        "artifacts": {
            "output_dir": relative_to_repo(output_dir),
            "server_stdout": relative_to_repo(server.stdout_path),
            "server_stderr": relative_to_repo(server.stderr_path),
            "metadata": relative_to_repo(output_dir / "metadata.json"),
            "request": relative_to_repo(output_dir / "request.json"),
        },
        "measured_runs": [asdict(run) for run in measured_runs],
        "aggregate": summarize_runs(measured_runs),
    }
    write_json(output_dir / "summary.json", summary)
    print(f"wrote summary to {relative_to_repo(output_dir / 'summary.json')}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main(sys.argv[1:]))
    except HarnessError as exc:
        print(f"error: {exc}", file=sys.stderr)
        raise SystemExit(1)

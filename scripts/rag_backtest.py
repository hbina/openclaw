#!/usr/bin/env python3
"""Temporally backtest OpenClaw conversation RAG against historical messages.

The source database is opened read-only. Query embeddings are obtained from the
configured local embedding server and cached in a separate SQLite file. The
report never allows an exchange ending at or after the historical query turn to
become a candidate, preventing future-data leakage.

For each historical user message, the report exhaustively scores every older
indexed exchange that is eligible for archive recall. It lists every exchange
that passes OpenClaw's cosine threshold, rather than silently truncating the
report to an arbitrary top K. The two recent same-route exchanges are reported
separately because OpenClaw already supplies them as ordinary chat history.

This evaluates retrieval behavior, not answer quality. Use the generated
judgment template to mark exchanges that were genuinely useful; a later run
with --judgments then reports precision@K, recall@K, and reciprocal rank.
"""

from __future__ import annotations

import argparse
import hashlib
import html
import json
import math
import os
import sqlite3
import struct
import sys
import urllib.error
import urllib.request
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable
from urllib.parse import quote


QUERY_PREFIX = "task: search result | query: "
INDEX_VERSION = 1
DEFAULT_MIN_SCORE = 0.35
DEFAULT_RECENT_EXCHANGES = 2


@dataclass(frozen=True)
class Turn:
    id: int
    channel_id: str
    sender_id: str
    role: str
    content_type: str
    content: str
    created_at: str


@dataclass
class Exchange:
    start_id: int
    end_id: int
    channel_id: str
    sender_id: str
    created_at: str
    turns: list[Turn]
    embeddings: list[list[float]]


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", required=True, type=Path, help="Local copy of the OpenClaw SQLite database")
    parser.add_argument("--config", type=Path, help="Optional openclaw.json; normally unnecessary")
    parser.add_argument("--index-id", help="Corpus embedding index ID (inferred when unambiguous)")
    parser.add_argument("--dimensions", type=int, help="Corpus dimensions (inferred when unambiguous)")
    parser.add_argument("--embedding-url", help="Local embedding API base URL (default: http://127.0.0.1:8081/v1)")
    parser.add_argument("--embedding-model", help="Embedding API model name (default: default)")
    parser.add_argument("--output", type=Path, help="HTML report path (default: beside copied database)")
    parser.add_argument("--json-output", type=Path, help="Machine-readable result path (default: beside HTML)")
    parser.add_argument("--cache", type=Path, help="Separate query-embedding cache (default: beside HTML)")
    parser.add_argument("--judgments", type=Path, help="Existing human relevance judgments")
    parser.add_argument("--judgment-template", type=Path, help="Template output path (never overwritten)")
    parser.add_argument(
        "--top-k",
        type=int,
        default=10,
        help="Evaluation cutoff for precision/recall metrics; the report still lists every qualifying match",
    )
    parser.add_argument("--min-score", type=float, help="Override configured production threshold")
    parser.add_argument("--recent-exchanges", type=int, default=DEFAULT_RECENT_EXCHANGES)
    parser.add_argument("--batch-size", type=int, default=16, help="Queries per embedding request")
    parser.add_argument("--start-id", type=int, default=0, help="Skip query turns before this history ID")
    parser.add_argument("--limit", type=int, help="Maximum historical query turns")
    parser.add_argument("--include-scheduled", action="store_true", help="Also evaluate scheduled reminder events")
    parser.add_argument(
        "--api-key-env",
        default="OPENCLAW_EMBEDDING_API_KEY",
        help="Environment variable holding an optional local-server API key",
    )
    args = parser.parse_args()
    if args.top_k <= 0 or args.batch_size <= 0 or args.recent_exchanges < 0:
        parser.error("--top-k and --batch-size must be positive; --recent-exchanges must be non-negative")
    if args.min_score is not None and not 0 <= args.min_score <= 1:
        parser.error("--min-score must be between 0 and 1")
    return args


def read_config(path: Path | None) -> dict[str, Any]:
    if path is None:
        return {}
    with path.open("r", encoding="utf-8") as source:
        config = json.load(source)
    embedding = config["models"]["embeddings"]
    history = config.get("agents", {}).get("defaults", {}).get("historySearch", {})
    return {
        "base_url": str(embedding["baseUrl"]).rstrip("/"),
        "model": str(embedding["model"]),
        "index_id": str(embedding["indexId"]),
        "dimensions": int(embedding["dimensions"]),
        "min_score": float(history.get("minScore") or DEFAULT_MIN_SCORE),
    }


def resolve_settings(
    db: sqlite3.Connection, args: argparse.Namespace, file_config: dict[str, Any]
) -> dict[str, Any]:
    rows = db.execute(
        """SELECT embedding_model, index_version, dimensions, count(embedding) AS embedded
           FROM conversation_chunks
           WHERE embedding IS NOT NULL
           GROUP BY embedding_model, index_version, dimensions
           ORDER BY embedded DESC"""
    ).fetchall()
    if not rows:
        raise RuntimeError("the copied database has no conversation embeddings")

    requested_index = args.index_id or file_config.get("index_id")
    requested_dimensions = args.dimensions or file_config.get("dimensions")
    compatible = [
        row
        for row in rows
        if int(row["index_version"]) == INDEX_VERSION
        and (requested_index is None or row["embedding_model"] == requested_index)
        and (requested_dimensions is None or int(row["dimensions"]) == requested_dimensions)
    ]
    if not compatible:
        raise RuntimeError("no embedded corpus matches the requested index ID, version, and dimensions")
    if len(compatible) > 1 and requested_index is None and requested_dimensions is None:
        choices = ", ".join(
            f"{row['embedding_model']}/{row['dimensions']}d ({row['embedded']} chunks)"
            for row in compatible
        )
        raise RuntimeError(f"multiple embedding corpora found; choose --index-id and --dimensions: {choices}")
    corpus = compatible[0]
    return {
        "base_url": (args.embedding_url or file_config.get("base_url") or "http://127.0.0.1:8081/v1").rstrip("/"),
        "model": args.embedding_model or file_config.get("model") or "default",
        "index_id": str(corpus["embedding_model"]),
        "dimensions": int(corpus["dimensions"]),
        "min_score": float(file_config.get("min_score", DEFAULT_MIN_SCORE)),
    }


def open_readonly(path: Path) -> sqlite3.Connection:
    resolved = path.resolve(strict=True)
    connection = sqlite3.connect(f"file:{quote(str(resolved))}?mode=ro", uri=True)
    connection.row_factory = sqlite3.Row
    return connection


def unpack_vector(blob: bytes, dimensions: int) -> list[float]:
    expected = dimensions * 4
    if len(blob) != expected:
        raise ValueError(f"embedding has {len(blob)} bytes, expected {expected}")
    values = list(struct.unpack(f"<{dimensions}f", blob))
    if not all(math.isfinite(value) for value in values):
        raise ValueError("embedding contains a non-finite value")
    return values


def pack_vector(vector: list[float]) -> bytes:
    return struct.pack(f"<{len(vector)}f", *vector)


def normalize(values: Iterable[float], dimensions: int) -> list[float]:
    vector = [float(value) for value in values]
    if len(vector) != dimensions:
        raise ValueError(f"embedding has {len(vector)} dimensions, expected {dimensions}")
    if not all(math.isfinite(value) for value in vector):
        raise ValueError("embedding contains a non-finite value")
    norm = math.sqrt(sum(value * value for value in vector))
    if norm == 0:
        raise ValueError("embedding has zero norm")
    return [value / norm for value in vector]


def dot(left: list[float], right: list[float]) -> float:
    return sum(a * b for a, b in zip(left, right, strict=True))


def load_turns(db: sqlite3.Connection) -> list[Turn]:
    rows = db.execute(
        """SELECT id, channel_id, sender_id, role, content_type, content, created_at
           FROM conversation_history ORDER BY id"""
    )
    return [Turn(**dict(row)) for row in rows]


def load_exchanges(
    db: sqlite3.Connection, turns: list[Turn], index_id: str, dimensions: int
) -> list[Exchange]:
    turns_by_id = {turn.id: turn for turn in turns}
    grouped: dict[tuple[int, int], list[list[float]]] = {}
    rows = db.execute(
        """SELECT start_history_id, end_history_id, embedding
           FROM conversation_chunks
           WHERE embedding_model = ? AND index_version = ? AND dimensions = ?
             AND embedding IS NOT NULL
           ORDER BY start_history_id, end_history_id, part_index""",
        (index_id, INDEX_VERSION, dimensions),
    )
    for row in rows:
        key = (int(row["start_history_id"]), int(row["end_history_id"]))
        grouped.setdefault(key, []).append(unpack_vector(row["embedding"], dimensions))

    exchanges: list[Exchange] = []
    for (start_id, end_id), embeddings in grouped.items():
        first = turns_by_id.get(start_id)
        if first is None:
            continue
        # Exchanges are assembled independently per channel/sender route in
        # production. Global history IDs from another interleaved route can
        # therefore fall numerically inside this range and must not leak in.
        span = [
            turns_by_id[value]
            for value in range(start_id, end_id + 1)
            if value in turns_by_id
            and turns_by_id[value].channel_id == first.channel_id
            and turns_by_id[value].sender_id == first.sender_id
        ]
        if not span:
            continue
        exchanges.append(
            Exchange(
                start_id=start_id,
                end_id=end_id,
                channel_id=span[0].channel_id,
                sender_id=span[0].sender_id,
                created_at=span[0].created_at,
                turns=span,
                embeddings=embeddings,
            )
        )
    return sorted(exchanges, key=lambda item: item.start_id)


def render_reply_context(reply: dict[str, Any]) -> str:
    body = str(reply.get("body") or "").strip()
    if not body and reply.get("content_unavailable"):
        body = "[non-text Telegram message; content unavailable]"
    rendered = f"Reply context:\nAuthor: {reply.get('author', 'unknown')}\nMessage:\n{body}"
    selected = str(reply.get("selected_text") or "").strip()
    if selected:
        rendered += f"\n\nSelected text:\n{selected}"
    return rendered


def render_turn_content(turn: Turn) -> str:
    if turn.content_type == "inbound_message":
        inbound = json.loads(turn.content)
        content = str(inbound.get("content") or "")
        if inbound.get("reply"):
            return render_reply_context(inbound["reply"]) + f"\n\nCurrent user message:\n{content}"
        return content
    if turn.content_type == "scheduled_reminder":
        reminder = json.loads(turn.content)
        return str(reminder.get("message") or turn.content)
    if turn.content_type == "tool_call":
        message = json.loads(turn.content)
        calls = message.get("tool_calls") or message.get("toolCalls") or []
        rendered = []
        for call in calls:
            function = call.get("function", {})
            rendered.append(f"Assistant action {function.get('name')}: {function.get('arguments')}")
        return "\n".join(rendered) or turn.content
    if turn.content_type == "tool_result":
        result = json.loads(turn.content)
        return f"Tool result {result.get('name')}: {result.get('content')}"
    return turn.content


def render_exchange(exchange: Exchange) -> str:
    lines = []
    for turn in exchange.turns:
        if turn.content_type == "scheduled_reminder":
            label = "Scheduled reminder"
        elif turn.role == "user":
            label = "User"
        elif turn.role == "assistant":
            label = "Assistant"
        else:
            label = "Tool"
        lines.append(f"{label}: {render_turn_content(turn)}")
    return "\n".join(lines)


def select_queries(turns: list[Turn], include_scheduled: bool, start_id: int) -> list[tuple[Turn, str]]:
    accepted = {"text", "inbound_message"}
    if include_scheduled:
        accepted.add("scheduled_reminder")
    queries = []
    for turn in turns:
        if turn.id < start_id or turn.role != "user" or turn.content_type not in accepted:
            continue
        content = render_turn_content(turn).strip()
        if content:
            queries.append((turn, content))
    return queries


def open_cache(path: Path) -> sqlite3.Connection:
    path.parent.mkdir(parents=True, exist_ok=True)
    cache = sqlite3.connect(path)
    cache.execute(
        """CREATE TABLE IF NOT EXISTS query_embeddings (
               query_id INTEGER NOT NULL,
               content_hash TEXT NOT NULL,
               index_id TEXT NOT NULL,
               dimensions INTEGER NOT NULL,
               embedding BLOB NOT NULL,
               PRIMARY KEY (query_id, index_id, dimensions)
           )"""
    )
    return cache


def embed_request(
    base_url: str, model: str, inputs: list[str], dimensions: int, api_key: str
) -> list[list[float]]:
    payload = json.dumps({"model": model, "input": inputs, "encoding_format": "float"}).encode()
    request = urllib.request.Request(base_url + "/embeddings", data=payload, method="POST")
    request.add_header("Content-Type", "application/json")
    if api_key:
        request.add_header("Authorization", "Bearer " + api_key)
    try:
        with urllib.request.urlopen(request, timeout=120) as response:
            body = json.load(response)
    except urllib.error.HTTPError as error:
        detail = error.read(64 * 1024).decode("utf-8", errors="replace").strip()
        suffix = f": {detail}" if detail else ""
        raise RuntimeError(f"local embedding server returned HTTP {error.code}{suffix}") from error
    except (urllib.error.URLError, TimeoutError) as error:
        raise RuntimeError(f"local embedding request failed: {error}") from error
    data = body.get("data", [])
    if len(data) != len(inputs):
        raise RuntimeError(f"embedding server returned {len(data)} vectors for {len(inputs)} inputs")
    ordered: list[list[float] | None] = [None] * len(inputs)
    for item in data:
        index = int(item["index"])
        if index < 0 or index >= len(inputs) or ordered[index] is not None:
            raise RuntimeError(f"embedding server returned invalid index {index}")
        ordered[index] = normalize(item["embedding"], dimensions)
    if any(vector is None for vector in ordered):
        raise RuntimeError("embedding response omitted an input")
    return [vector for vector in ordered if vector is not None]


def query_embeddings(
    cache: sqlite3.Connection,
    queries: list[tuple[Turn, str]],
    config: dict[str, Any],
    api_key: str,
    batch_size: int,
) -> tuple[dict[int, list[float]], dict[int, str]]:
    result: dict[int, list[float]] = {}
    failures: dict[int, str] = {}
    missing: list[tuple[Turn, str, str]] = []
    for turn, content in queries:
        digest = hashlib.sha256(content.encode()).hexdigest()
        row = cache.execute(
            """SELECT content_hash, embedding FROM query_embeddings
               WHERE query_id = ? AND index_id = ? AND dimensions = ?""",
            (turn.id, config["index_id"], config["dimensions"]),
        ).fetchone()
        if row and row[0] == digest:
            result[turn.id] = unpack_vector(row[1], config["dimensions"])
        else:
            missing.append((turn, content, digest))

    completed = 0

    def process_batch(batch: list[tuple[Turn, str, str]]) -> None:
        nonlocal completed
        try:
            vectors = embed_request(
                config["base_url"],
                config["model"],
                [QUERY_PREFIX + content for _, content, _ in batch],
                config["dimensions"],
                api_key,
            )
        except RuntimeError as error:
            if len(batch) > 1:
                middle = len(batch) // 2
                process_batch(batch[:middle])
                process_batch(batch[middle:])
                return
            turn = batch[0][0]
            failures[turn.id] = str(error)
            completed += 1
            print(
                f"query history #{turn.id} unavailable: {error} "
                f"({completed}/{len(missing)} uncached queries)",
                file=sys.stderr,
            )
            return

        for (turn, _, digest), vector in zip(batch, vectors, strict=True):
            cache.execute(
                """INSERT INTO query_embeddings
                       (query_id, content_hash, index_id, dimensions, embedding)
                   VALUES (?, ?, ?, ?, ?)
                   ON CONFLICT(query_id, index_id, dimensions) DO UPDATE SET
                       content_hash=excluded.content_hash, embedding=excluded.embedding""",
                (turn.id, digest, config["index_id"], config["dimensions"], pack_vector(vector)),
            )
            result[turn.id] = vector
        cache.commit()
        completed += len(batch)
        print(f"processed {completed}/{len(missing)} uncached queries", file=sys.stderr)

    for start in range(0, len(missing), batch_size):
        process_batch(missing[start : start + batch_size])
    return result, failures


def load_judgments(path: Path | None) -> dict[int, dict[str, Any]]:
    if path is None:
        return {}
    with path.open("r", encoding="utf-8") as source:
        payload = json.load(source)
    return {int(item["query_id"]): item for item in payload.get("queries", [])}


def backtest(
    queries: list[tuple[Turn, str]],
    vectors: dict[int, list[float]],
    exchanges: list[Exchange],
    min_score: float,
    top_k: int,
    recent_count: int,
    judgments: dict[int, dict[str, Any]],
    failures: dict[int, str] | None = None,
) -> list[dict[str, Any]]:
    failures = failures or {}
    results = []
    for turn, content in queries:
        prior_same_route = [
            exchange
            for exchange in exchanges
            if exchange.end_id < turn.id
            and exchange.channel_id == turn.channel_id
            and exchange.sender_id == turn.sender_id
        ]
        recent = prior_same_route[-recent_count:] if recent_count else []
        excluded = {exchange.start_id for exchange in recent}
        scored = []
        if turn.id in vectors:
            for exchange in exchanges:
                if exchange.end_id >= turn.id or exchange.start_id in excluded:
                    continue
                score = max(dot(vectors[turn.id], embedding) for embedding in exchange.embeddings)
                scored.append((score, exchange))
        scored.sort(key=lambda item: (-item[0], item[1].start_id))
        matches = [(score, exchange) for score, exchange in scored if score >= min_score]
        evaluated = matches[:top_k]
        judgment = judgments.get(turn.id, {})
        relevant = {int(value) for value in judgment.get("relevant_exchange_start_ids", [])}
        retrieved = [exchange.start_id for _, exchange in evaluated]
        metrics = None
        if judgment.get("judged") is True:
            hits = [value for value in retrieved if value in relevant]
            first_rank = next((index + 1 for index, value in enumerate(retrieved) if value in relevant), None)
            metrics = {
                "precision_at_k": len(hits) / top_k,
                "recall_at_k": (len(hits) / len(relevant)) if relevant else None,
                "reciprocal_rank": (1 / first_rank) if first_rank else 0.0,
                "relevant_count": len(relevant),
            }
        results.append(
            {
                "query_id": turn.id,
                "created_at": turn.created_at,
                "channel_id": turn.channel_id,
                "sender_id": turn.sender_id,
                "query": content,
                "retrieval_error": failures.get(turn.id),
                "excluded_recent_start_ids": sorted(excluded),
                "recent_context": [
                    {
                        "start_id": exchange.start_id,
                        "end_id": exchange.end_id,
                        "created_at": exchange.created_at,
                        "channel_id": exchange.channel_id,
                        "conversation": render_exchange(exchange),
                    }
                    for exchange in recent
                ],
                "scanned_prior_exchange_count": len(scored),
                "scanned_prior_chunk_count": sum(len(exchange.embeddings) for _, exchange in scored),
                "above_threshold_count": len(matches),
                # Compact proof that the exhaustive scan considered every
                # eligible prior exchange. Full conversation text is repeated
                # only for qualifying matches to keep the JSON manageable.
                "all_prior_scores": [
                    {
                        "rank": rank,
                        "score": score,
                        "start_id": exchange.start_id,
                        "end_id": exchange.end_id,
                        "chunk_count": len(exchange.embeddings),
                        "passes_threshold": score >= min_score,
                    }
                    for rank, (score, exchange) in enumerate(scored, 1)
                ],
                "matches": [
                    {
                        "rank": rank,
                        "score": score,
                        "start_id": exchange.start_id,
                        "end_id": exchange.end_id,
                        "created_at": exchange.created_at,
                        "channel_id": exchange.channel_id,
                        "same_route": exchange.channel_id == turn.channel_id and exchange.sender_id == turn.sender_id,
                        "conversation": render_exchange(exchange),
                    }
                    for rank, (score, exchange) in enumerate(matches, 1)
                ],
                "metrics": metrics,
            }
        )
    return results


def aggregate_metrics(results: list[dict[str, Any]]) -> dict[str, Any]:
    judged = [item["metrics"] for item in results if item["metrics"] is not None]
    positive = [metric for metric in judged if metric["recall_at_k"] is not None]
    return {
        "judged_queries": len(judged),
        "positive_queries": len(positive),
        "mean_precision_at_k": sum(item["precision_at_k"] for item in judged) / len(judged) if judged else None,
        "mean_recall_at_k": sum(item["recall_at_k"] for item in positive) / len(positive) if positive else None,
        "mean_reciprocal_rank": sum(item["reciprocal_rank"] for item in positive) / len(positive) if positive else None,
    }


def write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")


def write_judgment_template(path: Path, results: list[dict[str, Any]]) -> None:
    if path.exists():
        print(f"preserving existing judgment template: {path}", file=sys.stderr)
        return
    payload = {
        "instructions": "Set judged=true and list every genuinely useful prior exchange start ID. Empty means no historical exchange was needed.",
        "queries": [
            {
                "query_id": result["query_id"],
                "query": result["query"],
                "judged": False,
                "relevant_exchange_start_ids": [],
                "notes": "",
            }
            for result in results
        ],
    }
    write_json(path, payload)


def fmt_metric(value: float | None) -> str:
    return "not measured" if value is None else f"{value:.3f}"


def write_html(path: Path, payload: dict[str, Any]) -> None:
    metadata = payload["metadata"]
    aggregate = payload["aggregate_metrics"]
    sections = []
    for result in payload["queries"]:
        recent_html = []
        for recent in result["recent_context"]:
            recent_html.append(
                f"<details><summary>exchange {recent['start_id']}–{recent['end_id']} · "
                f"{html.escape(recent['channel_id'])}</summary>"
                f"<pre>{html.escape(recent['conversation'])}</pre></details>"
            )
        match_html = []
        for match in result["matches"]:
            route = "same route" if match["same_route"] else "owner-global cross-route"
            match_html.append(
                f"<details><summary>#{match['rank']} · score {match['score']:.4f} · "
                f"exchange {match['start_id']}–{match['end_id']} · {html.escape(route)}</summary>"
                f"<pre>{html.escape(match['conversation'])}</pre></details>"
            )
        metric_text = "unjudged"
        if result["metrics"]:
            metric_text = (
                f"P@K {result['metrics']['precision_at_k']:.3f}; "
                f"R@K {fmt_metric(result['metrics']['recall_at_k'])}; "
                f"RR {result['metrics']['reciprocal_rank']:.3f}"
            )
        error_html = ""
        if result.get("retrieval_error"):
            error_html = (
                "<p class='error'><strong>RAG unavailable:</strong> "
                + html.escape(result["retrieval_error"])
                + "</p>"
            )
        sections.append(
            f"<section><h2>Query history #{result['query_id']}</h2>"
            f"<p class='meta'>{html.escape(result['created_at'])} · {html.escape(result['channel_id'])} · "
            f"scanned {result['scanned_prior_exchange_count']} prior exchanges / "
            f"{result['scanned_prior_chunk_count']} chunks · "
            f"{result['above_threshold_count']} archive matches · {html.escape(metric_text)}</p>"
            f"<pre class='query'>{html.escape(result['query'])}</pre>"
            f"{error_html}"
            f"<h3>Recent same-route context (included without RAG scoring)</h3>"
            f"{''.join(recent_html) if recent_html else '<p>No complete recent exchange was available.</p>'}"
            f"<h3>All qualifying archive matches (cosine rank order)</h3>"
            f"{''.join(match_html) if match_html else '<p>No match exceeded the threshold.</p>'}</section>"
        )
    document = f"""<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>OpenClaw RAG backtest</title>
<style>
body {{ font: 16px/1.45 system-ui, sans-serif; max-width: 1100px; margin: 2rem auto; padding: 0 1rem; color: #202124; }}
h1, h2 {{ line-height: 1.2; }} section {{ border-top: 1px solid #ccc; padding: 1.5rem 0; }}
pre {{ white-space: pre-wrap; background: #f6f7f8; padding: 1rem; border-radius: .4rem; overflow-wrap: anywhere; }}
.query {{ border-left: .35rem solid #5067d8; }} .meta {{ color: #5f6368; }} .error {{ color: #a32020; }}
details {{ margin: .65rem 0; }} summary {{ cursor: pointer; font-weight: 600; }}
</style></head><body>
<h1>OpenClaw temporal RAG backtest</h1>
<p>This local report may contain private conversation content. For every historical user message it exhaustively scores every eligible prior indexed exchange, with no future-data leakage. It lists every threshold-qualified archive match and the recent exchanges OpenClaw includes separately. It does not evaluate generated answer quality.</p>
<ul><li>Queries: {metadata['query_count']}</li><li>Corpus exchanges: {metadata['exchange_count']}</li>
<li>Model/index: {html.escape(metadata['index_id'])}</li><li>Dimensions: {metadata['dimensions']}</li>
<li>Minimum score: {metadata['min_score']}</li><li>Metric cutoff K: {metadata['top_k']}</li>
<li>Judged queries: {aggregate['judged_queries']}</li>
<li>Mean precision@K: {fmt_metric(aggregate['mean_precision_at_k'])}</li>
<li>Mean recall@K (queries with relevant history): {fmt_metric(aggregate['mean_recall_at_k'])}</li>
<li>Mean reciprocal rank: {fmt_metric(aggregate['mean_reciprocal_rank'])}</li></ul>
{''.join(sections)}</body></html>"""
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(document, encoding="utf-8")


def main() -> int:
    args = parse_args()
    output = args.output or args.db.with_name(args.db.stem + "-rag-backtest.html")
    json_output = args.json_output or output.with_suffix(".json")
    cache_path = args.cache or output.with_suffix(".query-embeddings.sqlite")
    judgment_template = args.judgment_template or output.with_suffix(".judgments.json")
    api_key = os.environ.get(args.api_key_env, "")

    file_config = read_config(args.config)
    with open_readonly(args.db) as db:
        config = resolve_settings(db, args, file_config)
        turns = load_turns(db)
        exchanges = load_exchanges(db, turns, config["index_id"], config["dimensions"])
    min_score = config["min_score"] if args.min_score is None else args.min_score
    queries = select_queries(turns, args.include_scheduled, args.start_id)
    if args.limit is not None:
        queries = queries[: args.limit]
    if not exchanges:
        raise RuntimeError("no complete embeddings matched the configured index ID/version/dimensions")
    if not queries:
        raise RuntimeError("no historical user queries matched the selection")

    with open_cache(cache_path) as cache:
        vectors, failures = query_embeddings(cache, queries, config, api_key, args.batch_size)
    judgments = load_judgments(args.judgments)
    results = backtest(
        queries, vectors, exchanges, min_score, args.top_k, args.recent_exchanges, judgments, failures
    )
    payload = {
        "metadata": {
            "generated_at": datetime.now(timezone.utc).isoformat(),
            "source_db": str(args.db),
            "index_id": config["index_id"],
            "dimensions": config["dimensions"],
            "index_version": INDEX_VERSION,
            "min_score": min_score,
            "top_k": args.top_k,
            "recent_exchanges_excluded": args.recent_exchanges,
            "query_count": len(results),
            "retrieval_error_count": len(failures),
            "exchange_count": len(exchanges),
            "temporal_rule": "candidate.end_history_id < query.id",
            "note": (
                "matches lists every exchange passing the production cosine threshold; top_k only controls metrics. "
                "Production OpenClaw inserts the highest-scoring prefix that fits its live chat prompt capacity."
            ),
        },
        "aggregate_metrics": aggregate_metrics(results),
        "queries": results,
    }
    write_json(json_output, payload)
    write_html(output, payload)
    write_judgment_template(judgment_template, results)
    print(f"wrote {output}")
    print(f"wrote {json_output}")
    print(f"judgments: {judgment_template}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (KeyError, ValueError, RuntimeError, sqlite3.Error, OSError) as error:
        print(f"rag_backtest: {error}", file=sys.stderr)
        raise SystemExit(1)

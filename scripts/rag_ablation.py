#!/usr/bin/env python3
"""Compare four OpenClaw conversation-RAG document representations.

A uses the embeddings already stored by OpenClaw. B removes the conversation
title, timestamp, and channel. C additionally removes tool calls/results. D
uses only the initiating user-side text. Query embeddings, temporal eligibility,
recent-context exclusion, cosine scoring, and the score threshold are held
constant across all four variants.

The source database is read-only. Alternative corpus and query embeddings are
cached in separate SQLite files. The HTML report contains aggregate retrieval
volume, judged precision/recall at K, known-unrelated control similarities, and
per-query top-K results.
"""

from __future__ import annotations

import argparse
import hashlib
import html
import json
import math
import os
import sqlite3
import statistics
import sys
import urllib.error
import urllib.request
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import rag_backtest as baseline


MAX_DOCUMENT_TOKENS = 480
DOCUMENT_TOKEN_OVERLAP = 100
SHORT_DOCUMENT_CHARS = 1200
THRESHOLD_SWEEP = (0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65, 0.70)

VARIANTS = {
    "A": "Current complete exchange with title, timestamp, channel, tools, and assistant text",
    "B": "Complete exchange without title, timestamp, or channel",
    "C": "User and assistant text only; no title or tool calls/results",
    "D": "User-side message only; no title, assistant text, or tools",
    "E": "Independent user and assistant message vectors, grouped back into exchanges",
}

MESSAGE_EXPERIMENT_TABLE = "rag_message_embedding_test"
MESSAGE_INDEX_VERSION = 1


@dataclass
class VariantCorpus:
    code: str
    exchanges: list[baseline.Exchange]
    failed_exchange_ids: dict[int, str]
    embedding_metadata: dict[int, list[dict[str, Any]]] | None = None


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", required=True, type=Path, help="Local copy of the OpenClaw SQLite database")
    parser.add_argument("--config", type=Path, help="Optional openclaw.json")
    parser.add_argument("--index-id", help="Corpus embedding index ID (inferred when unambiguous)")
    parser.add_argument("--dimensions", type=int, help="Corpus dimensions (inferred when unambiguous)")
    parser.add_argument("--embedding-url", help="Local embedding API base URL")
    parser.add_argument("--embedding-model", help="Embedding API model name")
    parser.add_argument("--output", type=Path, help="HTML output path")
    parser.add_argument("--json-output", type=Path, help="Machine-readable output path")
    parser.add_argument("--cache", type=Path, help="Alternative-corpus embedding cache")
    parser.add_argument("--query-cache", type=Path, help="Query embedding cache")
    parser.add_argument("--judgments", type=Path, help="Human relevance judgments")
    parser.add_argument("--controls", type=Path, help="Known-unrelated exchange-pair controls")
    parser.add_argument("--min-score", type=float, help="Override configured production threshold")
    parser.add_argument("--top-k", type=int, default=5, help="Retrieval and metric cutoff")
    parser.add_argument("--recent-exchanges", type=int, default=baseline.DEFAULT_RECENT_EXCHANGES)
    parser.add_argument("--batch-size", type=int, default=16)
    parser.add_argument("--start-id", type=int, default=0)
    parser.add_argument("--limit", type=int)
    parser.add_argument("--api-key-env", default="OPENCLAW_EMBEDDING_API_KEY")
    args = parser.parse_args()
    if args.top_k <= 0 or args.batch_size <= 0 or args.recent_exchanges < 0:
        parser.error("--top-k and --batch-size must be positive; --recent-exchanges must be non-negative")
    if args.min_score is not None and not 0 <= args.min_score <= 1:
        parser.error("--min-score must be between 0 and 1")
    return args


def settings_args(args: argparse.Namespace) -> SimpleNamespace:
    return SimpleNamespace(
        index_id=args.index_id,
        dimensions=args.dimensions,
        embedding_url=args.embedding_url,
        embedding_model=args.embedding_model,
    )


def render_variant_body(exchange: baseline.Exchange, variant: str) -> str:
    if variant == "B":
        return baseline.render_exchange(exchange)
    lines: list[str] = []
    for turn in exchange.turns:
        if turn.content_type not in {"text", "inbound_message", "scheduled_reminder"}:
            continue
        if variant == "D" and turn.role != "user" and turn.content_type != "scheduled_reminder":
            continue
        if turn.content_type == "scheduled_reminder":
            label = "Scheduled reminder"
        elif turn.role == "user":
            label = "User"
        elif turn.role == "assistant":
            label = "Assistant"
        else:
            continue
        lines.append(f"{label}: {baseline.render_turn_content(turn)}")
    return "\n".join(lines).strip()


def server_url(base_url: str) -> str:
    return base_url[:-3] if base_url.endswith("/v1") else base_url


def local_json(endpoint: str, payload: dict[str, Any], api_key: str) -> dict[str, Any]:
    request = urllib.request.Request(
        endpoint,
        data=json.dumps(payload).encode(),
        method="POST",
        headers={"Content-Type": "application/json"},
    )
    if api_key:
        request.add_header("Authorization", "Bearer " + api_key)
    try:
        with urllib.request.urlopen(request, timeout=120) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        detail = error.read(64 * 1024).decode("utf-8", errors="replace").strip()
        raise RuntimeError(f"local embedding utility returned HTTP {error.code}: {detail}") from error
    except (urllib.error.URLError, TimeoutError) as error:
        raise RuntimeError(f"local embedding utility request failed: {error}") from error


def tokenize(base_url: str, content: str, api_key: str) -> list[int]:
    response = local_json(
        server_url(base_url) + "/tokenize",
        {"content": content, "add_special": False, "parse_special": True},
        api_key,
    )
    return [int(token) for token in response.get("tokens", [])]


def detokenize(base_url: str, tokens: list[int], api_key: str) -> str:
    response = local_json(server_url(base_url) + "/detokenize", {"tokens": tokens}, api_key)
    return str(response.get("content") or "")


def variant_documents(
    exchange: baseline.Exchange,
    variant: str,
    base_url: str,
    api_key: str,
) -> list[str]:
    body = render_variant_body(exchange, variant)
    return chunk_document_body(body, base_url, api_key)


def chunk_document_body(body: str, base_url: str, api_key: str) -> list[str]:
    if not body:
        return []
    prefix = "text: "
    document = prefix + body
    if len(document) <= SHORT_DOCUMENT_CHARS:
        return [document]
    document_tokens = tokenize(base_url, document, api_key)
    if len(document_tokens) <= MAX_DOCUMENT_TOKENS:
        return [document]
    prefix_tokens = tokenize(base_url, prefix, api_key)
    body_tokens = tokenize(base_url, body, api_key)
    window = MAX_DOCUMENT_TOKENS - len(prefix_tokens)
    if window <= DOCUMENT_TOKEN_OVERLAP:
        raise RuntimeError("alternative document prefix consumes the embedding window")
    step = window - DOCUMENT_TOKEN_OVERLAP
    documents = []
    for start in range(0, len(body_tokens), step):
        end = min(start + window, len(body_tokens))
        documents.append(prefix + detokenize(base_url, body_tokens[start:end], api_key))
        if end == len(body_tokens):
            break
    return documents


def message_content(turn: baseline.Turn) -> tuple[str, str] | None:
    if turn.role == "user" and turn.content_type == "inbound_message":
        inbound = json.loads(turn.content)
        content = str(inbound.get("content") or "").strip()
        return ("user", content) if content else None
    if turn.role == "user" and turn.content_type == "text":
        content = turn.content.strip()
        return ("user", content) if content else None
    if turn.role == "assistant" and turn.content_type == "text":
        content = turn.content.strip()
        return ("assistant", content) if content else None
    return None


def open_corpus_cache(path: Path) -> sqlite3.Connection:
    path.parent.mkdir(parents=True, exist_ok=True)
    cache = sqlite3.connect(path)
    cache.row_factory = sqlite3.Row
    cache.execute(
        """CREATE TABLE IF NOT EXISTS corpus_embeddings (
               variant TEXT NOT NULL,
               start_id INTEGER NOT NULL,
               end_id INTEGER NOT NULL,
               part_index INTEGER NOT NULL,
               content_hash TEXT NOT NULL,
               index_id TEXT NOT NULL,
               dimensions INTEGER NOT NULL,
               embedding BLOB NOT NULL,
               PRIMARY KEY (variant, start_id, end_id, part_index, index_id, dimensions)
           )"""
    )
    return cache


def load_or_embed_variant(
    cache: sqlite3.Connection,
    source: list[baseline.Exchange],
    variant: str,
    config: dict[str, Any],
    api_key: str,
    batch_size: int,
) -> VariantCorpus:
    documents: list[tuple[baseline.Exchange, int, str, str]] = []
    failures: dict[int, str] = {}
    for position, exchange in enumerate(source, 1):
        try:
            parts = variant_documents(exchange, variant, config["base_url"], api_key)
        except RuntimeError as error:
            failures[exchange.start_id] = str(error)
            continue
        for part_index, document in enumerate(parts):
            digest = hashlib.sha256(document.encode()).hexdigest()
            documents.append((exchange, part_index, document, digest))
        if position % 32 == 0 or position == len(source):
            print(f"prepared variant {variant}: {position}/{len(source)} exchanges", file=sys.stderr)

    vectors: dict[tuple[int, int], list[float]] = {}
    missing: list[tuple[baseline.Exchange, int, str, str]] = []
    for exchange, part_index, document, digest in documents:
        row = cache.execute(
            """SELECT content_hash, embedding FROM corpus_embeddings
               WHERE variant=? AND start_id=? AND end_id=? AND part_index=?
                 AND index_id=? AND dimensions=?""",
            (
                variant,
                exchange.start_id,
                exchange.end_id,
                part_index,
                config["index_id"],
                config["dimensions"],
            ),
        ).fetchone()
        if row and row["content_hash"] == digest:
            vectors[(exchange.start_id, part_index)] = baseline.unpack_vector(
                row["embedding"], config["dimensions"]
            )
        else:
            missing.append((exchange, part_index, document, digest))

    completed = 0

    def embed_batch(batch: list[tuple[baseline.Exchange, int, str, str]]) -> None:
        nonlocal completed
        if not batch:
            return
        try:
            embedded = baseline.embed_request(
                config["base_url"],
                config["model"],
                [document for _, _, document, _ in batch],
                config["dimensions"],
                api_key,
            )
        except RuntimeError as error:
            if len(batch) > 1:
                middle = len(batch) // 2
                embed_batch(batch[:middle])
                embed_batch(batch[middle:])
                return
            exchange = batch[0][0]
            failures[exchange.start_id] = str(error)
            completed += 1
            print(
                f"variant {variant} exchange {exchange.start_id} unavailable: {error} "
                f"({completed}/{len(missing)} uncached parts)",
                file=sys.stderr,
            )
            return
        for (exchange, part_index, _, digest), vector in zip(batch, embedded, strict=True):
            cache.execute(
                """INSERT INTO corpus_embeddings
                       (variant, start_id, end_id, part_index, content_hash, index_id, dimensions, embedding)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                   ON CONFLICT(variant, start_id, end_id, part_index, index_id, dimensions)
                   DO UPDATE SET content_hash=excluded.content_hash, embedding=excluded.embedding""",
                (
                    variant,
                    exchange.start_id,
                    exchange.end_id,
                    part_index,
                    digest,
                    config["index_id"],
                    config["dimensions"],
                    baseline.pack_vector(vector),
                ),
            )
            vectors[(exchange.start_id, part_index)] = vector
        cache.commit()
        completed += len(batch)
        print(
            f"embedded variant {variant}: {completed}/{len(missing)} uncached parts",
            file=sys.stderr,
        )

    for start in range(0, len(missing), batch_size):
        embed_batch(missing[start : start + batch_size])

    result = []
    for exchange in source:
        parts = [
            vector
            for (start_id, _), vector in sorted(vectors.items())
            if start_id == exchange.start_id
        ]
        if parts and exchange.start_id not in failures:
            result.append(
                baseline.Exchange(
                    start_id=exchange.start_id,
                    end_id=exchange.end_id,
                    channel_id=exchange.channel_id,
                    sender_id=exchange.sender_id,
                    created_at=exchange.created_at,
                    turns=exchange.turns,
                    embeddings=parts,
                )
            )
    return VariantCorpus(code=variant, exchanges=result, failed_exchange_ids=failures)


def load_or_embed_message_variant(
    db_path: Path,
    source: list[baseline.Exchange],
    config: dict[str, Any],
    api_key: str,
    batch_size: int,
) -> VariantCorpus:
    connection = sqlite3.connect(db_path.resolve(strict=True))
    connection.row_factory = sqlite3.Row
    try:
        connection.execute(
            f"""CREATE TABLE IF NOT EXISTS {MESSAGE_EXPERIMENT_TABLE} (
                   embedding_model   TEXT NOT NULL,
                   index_version     INTEGER NOT NULL,
                   dimensions        INTEGER NOT NULL,
                   history_id        INTEGER NOT NULL,
                   exchange_start_id INTEGER NOT NULL,
                   exchange_end_id   INTEGER NOT NULL,
                   message_kind      TEXT NOT NULL CHECK (message_kind IN ('user', 'assistant')),
                   part_index        INTEGER NOT NULL,
                   content_hash      TEXT NOT NULL,
                   content           TEXT NOT NULL,
                   document          TEXT NOT NULL,
                   embedding         BLOB NOT NULL,
                   PRIMARY KEY (embedding_model, index_version, dimensions, history_id, part_index)
               )"""
        )
        connection.execute(
            f"""CREATE INDEX IF NOT EXISTS idx_{MESSAGE_EXPERIMENT_TABLE}_exchange
                ON {MESSAGE_EXPERIMENT_TABLE}
                   (embedding_model, index_version, dimensions, exchange_start_id, exchange_end_id)"""
        )
        connection.commit()

        prepared: list[tuple[baseline.Exchange, baseline.Turn, str, int, str, str, str]] = []
        failures: dict[int, str] = {}
        for position, exchange in enumerate(source, 1):
            for turn in exchange.turns:
                rendered = message_content(turn)
                if rendered is None:
                    continue
                kind, content = rendered
                try:
                    parts = chunk_document_body(content, config["base_url"], api_key)
                except RuntimeError as error:
                    failures[exchange.start_id] = str(error)
                    continue
                for part_index, document in enumerate(parts):
                    digest = hashlib.sha256(document.encode()).hexdigest()
                    prepared.append((exchange, turn, kind, part_index, content, document, digest))
            if position % 32 == 0 or position == len(source):
                print(f"prepared variant E: {position}/{len(source)} exchanges", file=sys.stderr)

        vectors: dict[tuple[int, int], list[float]] = {}
        missing: list[tuple[baseline.Exchange, baseline.Turn, str, int, str, str, str]] = []
        for exchange, turn, kind, part_index, content, document, digest in prepared:
            row = connection.execute(
                f"""SELECT content_hash, embedding FROM {MESSAGE_EXPERIMENT_TABLE}
                    WHERE embedding_model=? AND index_version=? AND dimensions=?
                      AND history_id=? AND part_index=?""",
                (
                    config["index_id"],
                    MESSAGE_INDEX_VERSION,
                    config["dimensions"],
                    turn.id,
                    part_index,
                ),
            ).fetchone()
            if row and row["content_hash"] == digest:
                vectors[(turn.id, part_index)] = baseline.unpack_vector(
                    row["embedding"], config["dimensions"]
                )
            else:
                missing.append((exchange, turn, kind, part_index, content, document, digest))

        completed = 0

        def embed_batch(
            batch: list[tuple[baseline.Exchange, baseline.Turn, str, int, str, str, str]]
        ) -> None:
            nonlocal completed
            if not batch:
                return
            try:
                embedded = baseline.embed_request(
                    config["base_url"],
                    config["model"],
                    [document for _, _, _, _, _, document, _ in batch],
                    config["dimensions"],
                    api_key,
                )
            except RuntimeError as error:
                if len(batch) > 1:
                    middle = len(batch) // 2
                    embed_batch(batch[:middle])
                    embed_batch(batch[middle:])
                    return
                exchange = batch[0][0]
                failures[exchange.start_id] = str(error)
                completed += 1
                print(
                    f"variant E exchange {exchange.start_id} unavailable: {error} "
                    f"({completed}/{len(missing)} uncached parts)",
                    file=sys.stderr,
                )
                return
            for item, vector in zip(batch, embedded, strict=True):
                exchange, turn, kind, part_index, content, document, digest = item
                connection.execute(
                    f"""INSERT INTO {MESSAGE_EXPERIMENT_TABLE} (
                           embedding_model, index_version, dimensions, history_id,
                           exchange_start_id, exchange_end_id, message_kind, part_index,
                           content_hash, content, document, embedding
                       ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                       ON CONFLICT(embedding_model, index_version, dimensions, history_id, part_index)
                       DO UPDATE SET
                           exchange_start_id=excluded.exchange_start_id,
                           exchange_end_id=excluded.exchange_end_id,
                           message_kind=excluded.message_kind,
                           content_hash=excluded.content_hash,
                           content=excluded.content,
                           document=excluded.document,
                           embedding=excluded.embedding""",
                    (
                        config["index_id"],
                        MESSAGE_INDEX_VERSION,
                        config["dimensions"],
                        turn.id,
                        exchange.start_id,
                        exchange.end_id,
                        kind,
                        part_index,
                        digest,
                        content,
                        document,
                        baseline.pack_vector(vector),
                    ),
                )
                vectors[(turn.id, part_index)] = vector
            connection.commit()
            completed += len(batch)
            print(f"embedded variant E: {completed}/{len(missing)} uncached parts", file=sys.stderr)

        for start in range(0, len(missing), batch_size):
            embed_batch(missing[start : start + batch_size])

        by_exchange: dict[int, list[list[float]]] = {}
        metadata_by_key = {
            (turn.id, part_index): {
                "history_id": turn.id,
                "message_kind": kind,
                "content": content,
                "part_index": part_index,
            }
            for _, turn, kind, part_index, content, _, _ in prepared
        }
        metadata_by_exchange: dict[int, list[dict[str, Any]]] = {}
        turn_to_exchange = {
            turn.id: exchange.start_id for exchange in source for turn in exchange.turns
        }
        for (history_id, part_index), vector in sorted(vectors.items()):
            start_id = turn_to_exchange.get(history_id)
            if start_id is not None:
                by_exchange.setdefault(start_id, []).append(vector)
                metadata_by_exchange.setdefault(start_id, []).append(
                    metadata_by_key[(history_id, part_index)]
                )
        result = [
            baseline.Exchange(
                start_id=exchange.start_id,
                end_id=exchange.end_id,
                channel_id=exchange.channel_id,
                sender_id=exchange.sender_id,
                created_at=exchange.created_at,
                turns=exchange.turns,
                embeddings=by_exchange[exchange.start_id],
            )
            for exchange in source
            if by_exchange.get(exchange.start_id)
        ]
        return VariantCorpus(
            code="E",
            exchanges=result,
            failed_exchange_ids=failures,
            embedding_metadata=metadata_by_exchange,
        )
    finally:
        connection.close()


def percentile(values: list[float], fraction: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    position = (len(ordered) - 1) * fraction
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return ordered[lower]
    return ordered[lower] * (upper - position) + ordered[upper] * (position - lower)


def load_controls(path: Path | None) -> list[dict[str, Any]]:
    if path is None:
        return []
    with path.open("r", encoding="utf-8") as source:
        payload = json.load(source)
    return list(payload.get("pairs", []))


def load_method(path: Path | None) -> str:
    if path is None:
        return ""
    with path.open("r", encoding="utf-8") as source:
        return str(json.load(source).get("method") or "")


def score_exchange_pair(left: baseline.Exchange, right: baseline.Exchange) -> float:
    return max(baseline.dot(a, b) for a in left.embeddings for b in right.embeddings)


def evaluate_controls(
    corpora: dict[str, VariantCorpus], controls: list[dict[str, Any]]
) -> tuple[list[dict[str, Any]], dict[str, dict[str, float | None]]]:
    lookups = {
        code: {exchange.start_id: exchange for exchange in corpus.exchanges}
        for code, corpus in corpora.items()
    }
    results = []
    for control in controls:
        left_id = int(control["left_start_id"])
        right_id = int(control["right_start_id"])
        scores = {}
        for code in VARIANTS:
            left = lookups[code].get(left_id)
            right = lookups[code].get(right_id)
            scores[code] = score_exchange_pair(left, right) if left and right else None
        results.append({**control, "scores": scores})
    summary = {}
    for code in VARIANTS:
        values = [item["scores"][code] for item in results if item["scores"][code] is not None]
        summary[code] = {
            "mean": statistics.fmean(values) if values else None,
            "median": statistics.median(values) if values else None,
            "p90": percentile(values, 0.9),
            "max": max(values) if values else None,
        }
    return results, summary


def evaluate_queries(
    queries: list[tuple[baseline.Turn, str]],
    query_vectors: dict[int, list[float]],
    query_failures: dict[int, str],
    corpora: dict[str, VariantCorpus],
    min_score: float,
    top_k: int,
    recent_count: int,
    judgments: dict[int, dict[str, Any]],
) -> list[dict[str, Any]]:
    results = []
    for turn, content in queries:
        query_result: dict[str, Any] = {
            "query_id": turn.id,
            "created_at": turn.created_at,
            "query": content,
            "retrieval_error": query_failures.get(turn.id),
            "variants": {},
        }
        judgment = judgments.get(turn.id, {})
        relevant = {int(value) for value in judgment.get("relevant_exchange_start_ids", [])}
        for code, corpus in corpora.items():
            prior_same_route = [
                exchange
                for exchange in corpus.exchanges
                if exchange.end_id < turn.id
                and exchange.channel_id == turn.channel_id
                and exchange.sender_id == turn.sender_id
            ]
            excluded = {
                exchange.start_id for exchange in prior_same_route[-recent_count:]
            } if recent_count else set()
            scored: list[tuple[float, baseline.Exchange, dict[str, Any] | None]] = []
            if turn.id in query_vectors:
                for exchange in corpus.exchanges:
                    if exchange.end_id >= turn.id or exchange.start_id in excluded:
                        continue
                    part_scores = [
                        baseline.dot(query_vectors[turn.id], embedding)
                        for embedding in exchange.embeddings
                    ]
                    best_part = max(range(len(part_scores)), key=part_scores.__getitem__)
                    score = part_scores[best_part]
                    matched_message = (
                        corpus.embedding_metadata.get(exchange.start_id, [])[best_part]
                        if corpus.embedding_metadata
                        else None
                    )
                    if score >= min_score:
                        scored.append((score, exchange, matched_message))
            scored.sort(key=lambda item: (-item[0], item[1].start_id))
            top = scored[:top_k]
            retrieved = [exchange.start_id for _, exchange, _ in top]
            metrics = None
            if judgment.get("judged") is True:
                hits = [exchange_id for exchange_id in retrieved if exchange_id in relevant]
                first = next(
                    (rank for rank, exchange_id in enumerate(retrieved, 1) if exchange_id in relevant),
                    None,
                )
                metrics = {
                    "precision_at_k": len(hits) / top_k,
                    "recall_at_k": len(hits) / len(relevant) if relevant else None,
                    "reciprocal_rank": 1 / first if first else 0.0,
                    "relevant_count": len(relevant),
                }
            query_result["variants"][code] = {
                "eligible_exchange_count": sum(
                    1
                    for exchange in corpus.exchanges
                    if exchange.end_id < turn.id and exchange.start_id not in excluded
                ),
                "qualifying_count": len(scored),
                "top_matches": [
                    {
                        "rank": rank,
                        "score": score,
                        "start_id": exchange.start_id,
                        "end_id": exchange.end_id,
                        "matched_message": matched_message,
                        "conversation": baseline.render_exchange(exchange),
                    }
                    for rank, (score, exchange, matched_message) in enumerate(top, 1)
                ],
                "metrics": metrics,
            }
        results.append(query_result)
    return results


def aggregate(
    results: list[dict[str, Any]],
    corpora: dict[str, VariantCorpus],
    control_summary: dict[str, dict[str, float | None]],
    top_k: int,
) -> dict[str, dict[str, Any]]:
    aggregate_results = {}
    for code in VARIANTS:
        usable = [item for item in results if not item["retrieval_error"]]
        counts = [item["variants"][code]["qualifying_count"] for item in usable]
        metrics = [
            item["variants"][code]["metrics"]
            for item in usable
            if item["variants"][code]["metrics"] is not None
        ]
        positive = [item for item in metrics if item["recall_at_k"] is not None]
        negative_queries = [
            item
            for item in usable
            if item["variants"][code]["metrics"] is not None
            and item["variants"][code]["metrics"]["relevant_count"] == 0
        ]
        aggregate_results[code] = {
            "description": VARIANTS[code],
            "corpus_exchange_count": len(corpora[code].exchanges),
            "corpus_failure_count": len(corpora[code].failed_exchange_ids),
            "usable_query_count": len(usable),
            "mean_qualifying_count": statistics.fmean(counts) if counts else None,
            "median_qualifying_count": statistics.median(counts) if counts else None,
            "p90_qualifying_count": percentile([float(value) for value in counts], 0.9),
            "max_qualifying_count": max(counts) if counts else None,
            "queries_with_matches": sum(value > 0 for value in counts),
            "judged_query_count": len(metrics),
            "positive_judged_query_count": len(positive),
            "mean_precision_at_k": statistics.fmean(item["precision_at_k"] for item in metrics) if metrics else None,
            "positive_mean_precision_at_k": (
                statistics.fmean(item["precision_at_k"] for item in positive) if positive else None
            ),
            "mean_recall_at_k": statistics.fmean(item["recall_at_k"] for item in positive) if positive else None,
            "mean_reciprocal_rank": statistics.fmean(item["reciprocal_rank"] for item in positive) if positive else None,
            "negative_judged_query_count": len(negative_queries),
            "negative_queries_with_matches": sum(
                item["variants"][code]["qualifying_count"] > 0 for item in negative_queries
            ),
            "negative_query_retrieval_rate": (
                sum(item["variants"][code]["qualifying_count"] > 0 for item in negative_queries)
                / len(negative_queries)
                if negative_queries else None
            ),
            "negative_mean_returned_at_k": (
                statistics.fmean(
                    min(top_k, item["variants"][code]["qualifying_count"])
                    for item in negative_queries
                )
                if negative_queries else None
            ),
            "unrelated_controls": control_summary.get(code, {}),
        }
    return aggregate_results


def fmt(value: Any, digits: int = 3) -> str:
    if value is None:
        return "not measured"
    if isinstance(value, float):
        return f"{value:.{digits}f}"
    return str(value)


def write_html(path: Path, payload: dict[str, Any]) -> None:
    metadata = payload["metadata"]
    summary_rows = []
    for code, item in payload["aggregate"].items():
        unrelated = item["unrelated_controls"]
        summary_rows.append(
            "<tr>"
            f"<th>{code}</th><td>{html.escape(item['description'])}</td>"
            f"<td>{fmt(item['mean_qualifying_count'], 1)}</td>"
            f"<td>{fmt(item['median_qualifying_count'], 1)}</td>"
            f"<td>{fmt(item['p90_qualifying_count'], 1)}</td>"
            f"<td>{fmt(item['max_qualifying_count'])}</td>"
            f"<td>{fmt(item['positive_mean_precision_at_k'])}</td>"
            f"<td>{fmt(item['mean_recall_at_k'])}</td>"
            f"<td>{fmt(item['mean_reciprocal_rank'])}</td>"
            f"<td>{fmt(item['negative_query_retrieval_rate'])}</td>"
            f"<td>{fmt(item['negative_mean_returned_at_k'], 2)}</td>"
            f"<td>{fmt(unrelated.get('mean'))}</td>"
            f"<td>{fmt(unrelated.get('max'))}</td>"
            "</tr>"
        )

    controls = []
    for control in payload["controls"]:
        cells = "".join(f"<td>{fmt(control['scores'].get(code), 4)}</td>" for code in VARIANTS)
        controls.append(
            f"<tr><td>{html.escape(str(control.get('label', '')))}</td>"
            f"<td>{control['left_start_id']}</td><td>{control['right_start_id']}</td>{cells}</tr>"
        )

    sweep_rows = []
    for threshold, variants_at_threshold in payload["threshold_sweep"].items():
        for code, item in variants_at_threshold.items():
            sweep_rows.append(
                "<tr>"
                f"<td>{threshold}</td><th>{code}</th>"
                f"<td>{fmt(item['mean_qualifying_count'], 1)}</td>"
                f"<td>{fmt(item['median_qualifying_count'], 1)}</td>"
                f"<td>{fmt(item['positive_mean_precision_at_k'])}</td>"
                f"<td>{fmt(item['mean_recall_at_k'])}</td>"
                f"<td>{fmt(item['negative_query_retrieval_rate'])}</td>"
                f"<td>{fmt(item['negative_mean_returned_at_k'], 2)}</td>"
                "</tr>"
            )

    query_sections = []
    for query in payload["queries"]:
        variants = []
        for code in VARIANTS:
            result = query["variants"][code]
            matches = []
            for match in result["top_matches"]:
                matched_message = ""
                if match.get("matched_message"):
                    source = match["matched_message"]
                    matched_message = (
                        f"<p class='matched'><strong>Matched {html.escape(source['message_kind'])} "
                        f"message #{source['history_id']} (part {source['part_index']}):</strong></p>"
                        f"<pre class='matched'>{html.escape(source['content'])}</pre>"
                    )
                matches.append(
                    f"<details><summary>#{match['rank']} score {match['score']:.4f} · "
                    f"exchange {match['start_id']}–{match['end_id']}</summary>"
                    f"{matched_message}<pre>{html.escape(match['conversation'])}</pre></details>"
                )
            metric = result["metrics"]
            metric_text = "unjudged" if metric is None else (
                f"P@K {metric['precision_at_k']:.3f}; R@K {fmt(metric['recall_at_k'])}; "
                f"RR {metric['reciprocal_rank']:.3f}"
            )
            variants.append(
                f"<div class='variant'><h3>{code}: {html.escape(VARIANTS[code])}</h3>"
                f"<p class='meta'>{result['qualifying_count']} above threshold; {html.escape(metric_text)}</p>"
                f"{''.join(matches) if matches else '<p>No qualifying match.</p>'}</div>"
            )
        error = ""
        if query.get("retrieval_error"):
            error = f"<p class='error'>{html.escape(query['retrieval_error'])}</p>"
        query_sections.append(
            f"<section><h2>Query history #{query['query_id']}</h2>"
            f"<pre class='query'>{html.escape(query['query'])}</pre>{error}"
            f"<div class='grid'>{''.join(variants)}</div></section>"
        )

    document = f"""<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>OpenClaw RAG representation ablation</title>
<style>
body {{ font: 15px/1.45 system-ui,sans-serif; max-width: 1500px; margin: 2rem auto; padding: 0 1rem; color:#202124; }}
table {{ border-collapse: collapse; width:100%; }} th,td {{ border:1px solid #ccd0d5; padding:.45rem; text-align:left; vertical-align:top; }}
thead th {{ position:sticky; top:0; background:#eef1f4; }} section {{ border-top:2px solid #bbb; padding:1.25rem 0; }}
pre {{ white-space:pre-wrap; overflow-wrap:anywhere; background:#f6f7f8; padding:.8rem; border-radius:.35rem; }}
.query {{ border-left:.35rem solid #5067d8; }} .grid {{ display:grid; grid-template-columns:repeat(2,minmax(0,1fr)); gap:1rem; }}
.variant {{ border:1px solid #ddd; border-radius:.4rem; padding:.75rem; }} .meta {{ color:#5f6368; }} .error {{ color:#a32020; }}
.matched {{ border-left:.25rem solid #22936f; }} p.matched {{ padding-left:.5rem; }}
details {{ margin:.55rem 0; }} summary {{ cursor:pointer; font-weight:600; }}
@media(max-width:900px) {{ .grid {{ grid-template-columns:1fr; }} }}
</style></head><body>
<h1>OpenClaw RAG representation ablation</h1>
<p>This private local report holds query vectors and temporal candidate eligibility constant while changing only the indexed representation. A is the exact stored production corpus. B–D use generated exchange variants. E uses separate user and assistant message embeddings persisted in the copied database's <code>{MESSAGE_EXPERIMENT_TABLE}</code> table, then groups message hits back into their containing exchanges. Recent same-route exchanges are excluded consistently because OpenClaw already places them in normal chat context.</p>
<ul><li>Queries: {metadata['query_count']} ({metadata['query_failure_count']} embedding failures)</li>
<li>Threshold: {metadata['min_score']}</li><li>Evaluation cutoff: {metadata['top_k']}</li>
<li>Manually judged queries: {payload['aggregate']['A']['judged_query_count']} ({payload['aggregate']['A']['positive_judged_query_count']} with useful archive history; {payload['aggregate']['A']['negative_judged_query_count']} needing no archive)</li>
<li>Known-unrelated controls: {len(payload['controls'])}</li><li>Generated: {html.escape(metadata['generated_at'])}</li></ul>
<p><strong>Judgment method:</strong> {html.escape(metadata['judgment_method'] or 'No judgments supplied.')}</p>
<p><strong>Control method:</strong> {html.escape(metadata['control_method'] or 'No controls supplied.')}</p>
<h2>Aggregate comparison</h2>
<table><thead><tr><th>Variant</th><th>Representation</th><th>Mean matches</th><th>Median</th><th>P90</th><th>Max</th><th>Positive-query P@K</th><th>R@K</th><th>MRR</th><th>No-history retrieval rate</th><th>No-history mean returned</th><th>Unrelated mean</th><th>Unrelated max</th></tr></thead>
<tbody>{''.join(summary_rows)}</tbody></table>
<p><strong>Interpretation:</strong> lower unrelated-control similarity and fewer threshold-qualified matches indicate better separation, but relevance metrics are the deciding evidence. Positive-query P@K and R@K use only judged queries with useful archive history. “No-history” queries were judged to require no archive because they are self-contained or need authoritative live task, reminder, tool, or clock state; lower retrieval rate and fewer returned matches are better.</p>
<h2>Threshold sweep</h2>
<table><thead><tr><th>Threshold</th><th>Variant</th><th>Mean matches</th><th>Median</th><th>Positive-query P@K</th><th>R@K</th><th>No-history retrieval rate</th><th>No-history mean returned</th></tr></thead><tbody>{''.join(sweep_rows)}</tbody></table>
<h2>Known-unrelated control pairs</h2>
<table><thead><tr><th>Pair</th><th>Left exchange</th><th>Right exchange</th>{''.join(f'<th>{code}</th>' for code in VARIANTS)}</tr></thead><tbody>{''.join(controls)}</tbody></table>
<h2>Per-query comparison</h2>
{''.join(query_sections)}
</body></html>"""
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(document, encoding="utf-8")


def main() -> int:
    args = parse_args()
    output = args.output or args.db.with_name(args.db.stem + "-rag-ablation.html")
    json_output = args.json_output or output.with_suffix(".json")
    cache_path = args.cache or output.with_suffix(".corpus-embeddings.sqlite")
    query_cache_path = args.query_cache or output.with_suffix(".query-embeddings.sqlite")
    api_key = os.environ.get(args.api_key_env, "")

    file_config = baseline.read_config(args.config)
    with baseline.open_readonly(args.db) as db:
        config = baseline.resolve_settings(db, settings_args(args), file_config)
        turns = baseline.load_turns(db)
        production_exchanges = baseline.load_exchanges(
            db, turns, config["index_id"], config["dimensions"]
        )
    queries = baseline.select_queries(turns, False, args.start_id)
    if args.limit is not None:
        queries = queries[: args.limit]
    min_score = config["min_score"] if args.min_score is None else args.min_score
    if not production_exchanges or not queries:
        raise RuntimeError("no production exchanges or historical user queries were found")

    with baseline.open_cache(query_cache_path) as query_cache:
        query_vectors, query_failures = baseline.query_embeddings(
            query_cache, queries, config, api_key, args.batch_size
        )
    corpora = {
        "A": VariantCorpus("A", production_exchanges, {}),
    }
    with open_corpus_cache(cache_path) as corpus_cache:
        for code in ("B", "C", "D"):
            corpora[code] = load_or_embed_variant(
                corpus_cache,
                production_exchanges,
                code,
                config,
                api_key,
                args.batch_size,
            )
    corpora["E"] = load_or_embed_message_variant(
        args.db,
        production_exchanges,
        config,
        api_key,
        args.batch_size,
    )

    judgments = baseline.load_judgments(args.judgments)
    controls = load_controls(args.controls)
    control_results, control_summary = evaluate_controls(corpora, controls)
    results = evaluate_queries(
        queries,
        query_vectors,
        query_failures,
        corpora,
        min_score,
        args.top_k,
        args.recent_exchanges,
        judgments,
    )
    aggregate_results = aggregate(results, corpora, control_summary, args.top_k)
    threshold_sweep: dict[str, dict[str, Any]] = {}
    for threshold in sorted(set((*THRESHOLD_SWEEP, min_score))):
        if threshold == min_score:
            threshold_results = aggregate_results
        else:
            sweep_queries = evaluate_queries(
                queries,
                query_vectors,
                query_failures,
                corpora,
                threshold,
                args.top_k,
                args.recent_exchanges,
                judgments,
            )
            threshold_results = aggregate(
                sweep_queries, corpora, control_summary, args.top_k
            )
        threshold_sweep[f"{threshold:.2f}"] = threshold_results
    payload = {
        "metadata": {
            "generated_at": datetime.now(timezone.utc).isoformat(),
            "source_db": str(args.db),
            "index_id": config["index_id"],
            "dimensions": config["dimensions"],
            "min_score": min_score,
            "top_k": args.top_k,
            "query_count": len(results),
            "query_failure_count": len(query_failures),
            "recent_exchanges_excluded": args.recent_exchanges,
            "message_experiment_table": MESSAGE_EXPERIMENT_TABLE,
            "judgment_method": load_method(args.judgments),
            "control_method": load_method(args.controls),
        },
        "variants": VARIANTS,
        "aggregate": aggregate_results,
        "threshold_sweep": threshold_sweep,
        "controls": control_results,
        "queries": results,
    }
    baseline.write_json(json_output, payload)
    write_html(output, payload)
    print(f"wrote {output}")
    print(f"wrote {json_output}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (KeyError, ValueError, RuntimeError, sqlite3.Error, OSError) as error:
        print(f"rag_ablation: {error}", file=sys.stderr)
        raise SystemExit(1)

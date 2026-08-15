#!/usr/bin/env python3

import unittest

from rag_backtest import Exchange, Turn, backtest


def exchange(start, end, vector, channel="telegram", sender="owner", extra_vectors=None):
    return Exchange(
        start_id=start,
        end_id=end,
        channel_id=channel,
        sender_id=sender,
        created_at="2026-01-01 00:00:00",
        turns=[],
        embeddings=[vector, *(extra_vectors or [])],
    )


class BacktestTests(unittest.TestCase):
    def test_temporal_replay_excludes_future_and_two_recent_same_route_exchanges(self):
        query = Turn(
            id=100,
            channel_id="telegram",
            sender_id="owner",
            role="user",
            content_type="text",
            content="historical query",
            created_at="2026-01-02 00:00:00",
        )
        corpus = [
            exchange(1, 2, [1.0, 0.0]),
            exchange(11, 12, [1.0, 0.0]),
            exchange(21, 22, [1.0, 0.0]),
            exchange(31, 32, [0.8, 0.6], channel="cli"),
            exchange(41, 42, [0.0, 1.0], channel="http", extra_vectors=[[0.9, 0.0]]),
            exchange(101, 102, [1.0, 0.0]),
        ]

        results = backtest(
            [(query, query.content)],
            {query.id: [1.0, 0.0]},
            corpus,
            min_score=0.35,
            top_k=10,
            recent_count=2,
            judgments={query.id: {
                "judged": True,
                "relevant_exchange_start_ids": [41],
            }},
        )

        result = results[0]
        self.assertEqual(result["excluded_recent_start_ids"], [11, 21])
        self.assertEqual([item["start_id"] for item in result["recent_context"]], [11, 21])
        self.assertEqual(result["scanned_prior_exchange_count"], 3)
        self.assertEqual(result["scanned_prior_chunk_count"], 4)
        self.assertEqual([match["start_id"] for match in result["matches"]], [1, 41, 31])
        self.assertEqual([item["start_id"] for item in result["all_prior_scores"]], [1, 41, 31])
        self.assertTrue(all(match["end_id"] < query.id for match in result["matches"]))
        self.assertAlmostEqual(result["matches"][1]["score"], 0.9)
        self.assertAlmostEqual(result["metrics"]["recall_at_k"], 1.0)
        self.assertAlmostEqual(result["metrics"]["reciprocal_rank"], 0.5)

    def test_query_embedding_failure_is_reported_without_searching(self):
        query = Turn(
            id=10,
            channel_id="telegram",
            sender_id="owner",
            role="user",
            content_type="text",
            content="oversized query",
            created_at="2026-01-02 00:00:00",
        )
        result = backtest(
            [(query, query.content)],
            {},
            [exchange(1, 2, [1.0, 0.0])],
            min_score=0.35,
            top_k=10,
            recent_count=0,
            judgments={},
            failures={query.id: "input exceeds physical batch"},
        )[0]
        self.assertEqual(result["retrieval_error"], "input exceeds physical batch")
        self.assertEqual(result["scanned_prior_exchange_count"], 0)
        self.assertEqual(result["above_threshold_count"], 0)
        self.assertEqual(result["matches"], [])

    def test_top_k_limits_metrics_but_not_reported_matches(self):
        query = Turn(
            id=100,
            channel_id="telegram",
            sender_id="owner",
            role="user",
            content_type="text",
            content="historical query",
            created_at="2026-01-02 00:00:00",
        )
        corpus = [
            exchange(1, 2, [1.0, 0.0]),
            exchange(11, 12, [0.9, 0.0]),
            exchange(21, 22, [0.8, 0.0]),
        ]
        result = backtest(
            [(query, query.content)],
            {query.id: [1.0, 0.0]},
            corpus,
            min_score=0.35,
            top_k=1,
            recent_count=0,
            judgments={query.id: {
                "judged": True,
                "relevant_exchange_start_ids": [1, 11],
            }},
        )[0]

        self.assertEqual([match["start_id"] for match in result["matches"]], [1, 11, 21])
        self.assertEqual(result["metrics"]["precision_at_k"], 1.0)
        self.assertEqual(result["metrics"]["recall_at_k"], 0.5)


if __name__ == "__main__":
    unittest.main()

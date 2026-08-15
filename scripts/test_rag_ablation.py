#!/usr/bin/env python3

import unittest

from rag_ablation import (
    VariantCorpus,
    evaluate_controls,
    evaluate_queries,
    message_content,
    render_variant_body,
)
from rag_backtest import Exchange, Turn


def turn(id, role, content_type, content):
    return Turn(id, "telegram", "owner", role, content_type, content, "2026-01-01 00:00:00")


def exchange(start, vector, turns=None):
    return Exchange(start, start + 1, "telegram", "owner", "2026-01-01", turns or [], [vector])


class AblationTests(unittest.TestCase):
    def test_message_variant_uses_raw_inbound_content_and_excludes_tools(self):
        inbound = turn(
            1,
            "user",
            "inbound_message",
            '{"content":"current request","reply":{"body":"quoted boilerplate"}}',
        )
        tool = turn(2, "assistant", "tool_call", '{"tool_calls":[]}')
        answer = turn(3, "assistant", "text", "substantive answer")

        self.assertEqual(message_content(inbound), ("user", "current request"))
        self.assertIsNone(message_content(tool))
        self.assertEqual(message_content(answer), ("assistant", "substantive answer"))

    def test_cumulative_variant_rendering(self):
        item = exchange(1, [1.0, 0.0], [
            turn(1, "user", "text", "question"),
            turn(2, "assistant", "tool_call", '{"tool_calls":[]}'),
            turn(3, "tool", "tool_result", '{"name":"x","content":"result"}'),
            turn(4, "assistant", "text", "answer"),
        ])
        self.assertIn("Tool result", render_variant_body(item, "B"))
        self.assertEqual(render_variant_body(item, "C"), "User: question\nAssistant: answer")
        self.assertEqual(render_variant_body(item, "D"), "User: question")

    def test_query_evaluation_holds_temporal_and_recent_rules_constant(self):
        query = turn(20, "user", "text", "query")
        corpus = [exchange(1, [1.0, 0.0]), exchange(5, [0.8, 0.0]), exchange(10, [1.0, 0.0])]
        corpora = {code: VariantCorpus(code, corpus, {}) for code in "ABCDE"}
        result = evaluate_queries(
            [(query, "query")], {20: [1.0, 0.0]}, {}, corpora, 0.35, 1, 1,
            {20: {"judged": True, "relevant_exchange_start_ids": [1]}},
        )[0]
        for code in "ABCDE":
            variant = result["variants"][code]
            self.assertEqual(variant["eligible_exchange_count"], 2)
            self.assertEqual(variant["top_matches"][0]["start_id"], 1)
            self.assertEqual(variant["metrics"]["precision_at_k"], 1.0)

    def test_known_unrelated_pair_scores_each_variant(self):
        corpora = {
            code: VariantCorpus(code, [exchange(1, [1.0, 0.0]), exchange(5, [0.0, 1.0])], {})
            for code in "ABCDE"
        }
        controls, summary = evaluate_controls(
            corpora, [{"label": "unrelated", "left_start_id": 1, "right_start_id": 5}]
        )
        self.assertEqual(controls[0]["scores"], {code: 0.0 for code in "ABCDE"})
        self.assertEqual(summary["A"]["mean"], 0.0)


if __name__ == "__main__":
    unittest.main()

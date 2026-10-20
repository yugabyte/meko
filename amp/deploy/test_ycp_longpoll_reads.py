import unittest

from ycp_longpoll_reads import reads

LINE = (
    "time=2026-10-20T03:12:09Z level=INFO msg=request method=GET "
    'uri="/api/v1/agent/desired-state?since=41&wait={wait}" status=200 latency={latency} error=<nil>\n'
)


class Reads(unittest.TestCase):
    # The reconcile-agent's current wire form (AMP-103). Read as 0, it failed both
    # long-poll checks while the agent was long-polling fine.
    def test_whole_seconds(self):
        self.assertEqual(reads(LINE.format(wait="9", latency="9.001234567s")), [(9.0, 9.001234567)])

    def test_go_duration(self):
        self.assertEqual(reads(LINE.format(wait="9.873s", latency="250ms")), [(9.873, 0.25)])

    def test_other_requests_are_skipped(self):
        self.assertEqual(
            reads(LINE.format(wait="9", latency="1s").replace("desired-state", "status")), []
        )


if __name__ == "__main__":
    unittest.main()

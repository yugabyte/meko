import os
import sys

import pytest

PROJECT_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRC_DIR = os.path.join(PROJECT_ROOT, "src")
if SRC_DIR not in sys.path:
    sys.path.insert(0, SRC_DIR)


requires_real_controller = pytest.mark.skipif(
    not os.environ.get("AMP_CONTROLLER_URL"),
    reason="AMP_CONTROLLER_URL not set (real-controller round-trip skipped)",
)

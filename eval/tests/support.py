"""Where the tests find the harness and its fixtures. No network runs here."""

import os
import sys

TESTS = os.path.dirname(os.path.abspath(__file__))
EVAL = os.path.dirname(TESTS)
FIXTURES = os.path.join(TESTS, "fixtures")

if EVAL not in sys.path:
    sys.path.insert(0, EVAL)

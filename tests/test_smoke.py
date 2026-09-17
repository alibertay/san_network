"""Runs the end-to-end smoke suites as pytest tests (P0/P1/P2 regression)."""
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent
SCRIPTS = [
    "tests/smoke_p0.py",
    "tests/smoke_p1.py",
    "tests/smoke_p2.py",
    "tests/smoke_p3.py",
    "tests/smoke_p4.py",
    "tests/smoke_p5.py",
    "tests/smoke_p6.py",
    "tests/e2e_network.py",
    "tests/stress_consistency.py",
]


@pytest.mark.slow
@pytest.mark.parametrize("script", SCRIPTS)
def test_smoke_suite(script):
    result = subprocess.run(
        [sys.executable, script],
        cwd=ROOT,
        capture_output=True,
        text=True,
        timeout=600,
    )
    assert result.returncode == 0, (
        f"{script} failed\n--- stdout (tail) ---\n{result.stdout[-4000:]}\n"
        f"--- stderr (tail) ---\n{result.stderr[-2000:]}"
    )

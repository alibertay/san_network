"""Prometheus-style metrics rendered from a node snapshot.

Dependency-free: the text exposition format is produced directly so a public
node can be scraped by Prometheus without extra packages.
"""

from __future__ import annotations

COUNTER_NAMES = (
    "blocks_committed",
    "transactions_committed",
    "votes_received",
    "votes_seen",
    "vote_messages_received",
    "votes_dropped_invalid",
    "votes_dropped_height",
    "votes_dropped_voter",
    "votes_dropped_duplicate",
    "votes_dropped_equivocation",
    "votes_broadcast",
    "votes_unbroadcast",
    "reorgs",
    "slashing_events",
    "governance_changes",
)

GAUGE_NAMES = (
    "height",
    "finalized_height",
    "peers",
    "controllers",
    "mempool",
    "validators",
    "total_stake_units",
    "total_slashed",
    "total_burned",
    "base_fee",
    "contracts",
    "orphans",
)


def render_metrics(snapshot: dict) -> str:
    lines: list[str] = []

    for name in COUNTER_NAMES:
        metric = f"san_{name}"
        lines.append(f"# TYPE {metric} counter")
        lines.append(f"{metric} {int(snapshot.get(name, 0) or 0)}")

    for name in GAUGE_NAMES:
        metric = f"san_{name}"
        lines.append(f"# TYPE {metric} gauge")
        lines.append(f"{metric} {snapshot.get(name, 0)}")

    return "\n".join(lines) + "\n"

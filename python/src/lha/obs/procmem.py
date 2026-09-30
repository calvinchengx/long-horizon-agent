"""The process's peak resident memory, for the ``checkpoint`` event.

A long local mission lives in one process for days, so a structure that grows with every cycle
shows up as a peak RSS that climbs cycle after cycle; recording it with each checkpoint makes that
visible in the trace and the logs without a profiler.
"""

from __future__ import annotations

import resource
import sys


def peak_rss_mb() -> float:
    """Peak resident set size of this process in MiB (one decimal), 0.0 where unknown."""
    try:
        maxrss = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
    except (OSError, ValueError):
        return 0.0
    # Linux reports KiB, macOS bytes.
    scale = 1024.0 if sys.platform != "darwin" else 1.0
    return round(maxrss * scale / 2**20, 1)

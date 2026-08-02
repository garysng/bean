#!/usr/bin/env python3
"""Prints the average duration of each create phase between two metric scrapes.

A cumulative histogram's _sum/_count only gives a lifetime average, which is
useless for attributing a single stress run: 26 fast creates hide 16 slow ones.
Differencing two scrapes gives the average over just the interval, which is what
answers "where did the extra seconds go".

Usage: phase-delta.py before.txt after.txt
"""
import sys

PREFIX = "bean_node_create_phase_seconds"


def load(path):
    out = {}
    for line in open(path):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        key, _, value = line.rpartition(" ")
        out[key] = float(value)
    return out


def phase_of(key):
    # phase="agent_ready",runtime="fc"
    for part in key[key.find("{") + 1:key.rfind("}")].split(","):
        name, _, value = part.partition("=")
        if name == "phase":
            return value.strip('"')
    return "?"


def main():
    before, after = load(sys.argv[1]), load(sys.argv[2])
    rows = []
    for key, total in after.items():
        if not key.startswith(PREFIX + "_sum"):
            continue
        count_key = key.replace("_sum", "_count", 1)
        if count_key not in after:
            continue
        d_sum = total - before.get(key, 0.0)
        d_count = after[count_key] - before.get(count_key, 0.0)
        if d_count <= 0:
            continue
        rows.append((d_sum / d_count * 1000, int(d_count), phase_of(key)))

    rows.sort(reverse=True)
    for avg_ms, n, phase in rows:
        print(f"{phase:16s} n={n:3d}  avg={avg_ms:8.0f} ms")


if __name__ == "__main__":
    main()

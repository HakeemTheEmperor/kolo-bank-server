#!/usr/bin/env python3
"""Summarise stress-test results (JSON lines from run-go-tests.sh).

    python3 scripts/stress/summarize.py stress-results/before.jsonl
    python3 scripts/stress/summarize.py --k6 stress-results/k6

Prints one markdown table per test. Runs of the same scenario and
protection are merged; a metric that differed between runs is shown as
"min to max", never as an average or a best run.
"""
import glob
import json
import os
import re
import sys
from collections import OrderedDict


def flatten(prefix, value, out):
    if isinstance(value, dict):
        for k, v in sorted(value.items()):
            flatten(f"{prefix}.{k}" if prefix else k, v, out)
    else:
        out[prefix] = value


def fmt(values):
    if all(isinstance(v, bool) for v in values):
        return ", ".join(sorted({str(v).lower() for v in values}))
    nums = [v for v in values if isinstance(v, (int, float))]
    if len(nums) != len(values):
        return ", ".join(sorted({str(v) for v in values}))
    lo, hi = min(nums), max(nums)
    show = (lambda x: f"{x:.3f}".rstrip("0").rstrip(".")) if any(isinstance(v, float) for v in nums) else str
    return show(lo) if lo == hi else f"{show(lo)} to {show(hi)}"


def main(path):
    groups = OrderedDict()
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            r = json.loads(line)
            key = (r["test"], r["scenario"], r["protection"])
            flat = OrderedDict()
            flatten("", r["metrics"], flat)
            groups.setdefault(key, []).append(flat)

    by_test = OrderedDict()
    for (test, scenario, protection), rows in groups.items():
        by_test.setdefault(test, []).append((scenario, protection, rows))

    for test, entries in by_test.items():
        print(f"\n## {test}\n")
        for scenario, protection, rows in entries:
            metrics = []
            for row in rows:
                for k in row:
                    if k not in metrics:
                        metrics.append(k)
            print(f"**{scenario}** / `{protection}` ({len(rows)} run{'s' if len(rows) != 1 else ''})\n")
            print("| metric | value |\n|---|---|")
            for m in metrics:
                vals = [row[m] for row in rows if m in row]
                print(f"| {m} | {fmt(vals)} |")
            print()


def k6(root):
    """One table per configuration; labels ending in -runN are merged."""
    configs = OrderedDict()
    for d in sorted(glob.glob(os.path.join(root, "*"))):
        if not os.path.isdir(d):
            continue
        config = re.sub(r"-run\d+$", "", os.path.basename(d))
        for f in glob.glob(os.path.join(d, "summary-*.json")):
            vus = int(re.search(r"summary-(\d+)\.json$", f).group(1))
            m = json.load(open(f))["metrics"]
            dur = m["http_req_duration"]
            configs.setdefault(config, {}).setdefault(vus, []).append({
                "req/s": m["http_reqs"]["rate"],
                "p50 ms": dur["med"],
                "p95 ms": dur["p(95)"],
                "p99 ms": dur["p(99)"],
                "error %": m["http_req_failed"]["value"] * 100,
                "requests": m["http_reqs"]["count"],
            })
    cols = ["req/s", "p50 ms", "p95 ms", "p99 ms", "error %", "requests"]
    for config, levels in configs.items():
        runs = max(len(v) for v in levels.values())
        print(f"\n**{config}** ({runs} run{'s' if runs != 1 else ''} per level)\n")
        print("| VUs | " + " | ".join(cols) + " |")
        print("|---" * (len(cols) + 1) + "|")
        for vus in sorted(levels):
            rows = levels[vus]
            cells = []
            for c in cols:
                vals = [round(r[c]) if c != "error %" else round(r[c], 2) for r in rows]
                cells.append(fmt(vals))
            print(f"| {vus} | " + " | ".join(cells) + " |")


if __name__ == "__main__":
    if sys.argv[1] == "--k6":
        k6(sys.argv[2])
    else:
        main(sys.argv[1])

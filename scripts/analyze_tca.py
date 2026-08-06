#!/usr/bin/env python3
"""Chart the execution-cost study written by cmd/tcastudy.

Produces one figure with two panels:

  left  — cost vs order size, with arm C drawn as a BAND between the routed
          arm (zero replenishment) and the sliced arm (full replenishment),
          because the tape cannot say where inside that range reality sits.
  right — the same slippage numbers against the fee actually paid, on a log
          axis, which is the comparison that decides whether any of this
          matters.

Distributions, not just means: the median line carries a shaded inter-quartile
band so a reader can see the spread rather than trust a point.

Usage:  python scripts/analyze_tca.py tca.csv [-o tca.png]
"""

import argparse
import csv
import sys
from collections import defaultdict


def load(path):
    """Return {size_btc: {arm: [bps, ...]}}, skipping rows an arm failed on."""
    by_size = defaultdict(lambda: defaultdict(list))
    fees = defaultdict(list)
    with open(path, newline="") as fh:
        for r in csv.DictReader(fh):
            try:
                size = float(r["size_btc"])
            except (KeyError, ValueError):
                continue
            for arm, col, errcol in (
                ("naive", "naive_bps", "naive_err"),
                ("routed", "routed_bps", "routed_err"),
                # sliced* — each slice vs its OWN touch. This is the arm-C
                # number worth charting: the arrival-benchmarked one is
                # dominated by horizon price drift, not execution quality.
                ("sliced", "sliced_local_bps", "sliced_err"),
                ("shortfall", "sliced_bps", "sliced_err"),
            ):
                # An arm that refused (no depth, no book) must not be counted
                # as a zero-cost fill — that would silently flatter the curve.
                if r.get(errcol):
                    continue
                try:
                    by_size[size][arm].append(float(r[col]))
                except (KeyError, ValueError):
                    pass
            if not r.get("routed_err"):
                try:
                    fees[size].append(float(r["routed_fee_bps"]))
                except (KeyError, ValueError):
                    pass
    return by_size, fees


def quantiles(vals):
    """Return (p25, median, p75) without requiring numpy."""
    if not vals:
        return (float("nan"),) * 3
    v = sorted(vals)
    def q(f):
        return v[min(len(v) - 1, int(f * len(v)))]
    return q(0.25), q(0.50), q(0.75)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("csv", help="output of cmd/tcastudy")
    ap.add_argument("-o", "--out", default="tca.png")
    args = ap.parse_args()

    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except ImportError:
        sys.exit("matplotlib required: pip install matplotlib")

    by_size, fees = load(args.csv)
    if not by_size:
        sys.exit(f"no usable rows in {args.csv}")
    sizes = sorted(by_size)

    stats = {a: [quantiles(by_size[s][a]) for s in sizes]
             for a in ("naive", "routed", "sliced", "shortfall")}
    fee_med = [quantiles(fees[s])[1] for s in sizes]

    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(13, 5.5))

    colors = {"naive": "#c1440e", "routed": "#0b6e4f", "sliced": "#1d3557"}
    for arm in ("naive", "routed", "sliced"):
        med = [s[1] for s in stats[arm]]
        lo = [s[0] for s in stats[arm]]
        hi = [s[2] for s in stats[arm]]
        ax1.plot(sizes, med, marker="o", label=arm, color=colors[arm])
        ax1.fill_between(sizes, lo, hi, alpha=0.15, color=colors[arm])

    # The arm-C impact band: routed (no replenishment) to sliced (full).
    ax1.fill_between(
        sizes,
        [s[1] for s in stats["sliced"]],
        [s[1] for s in stats["routed"]],
        alpha=0.18, color="#457b9d", hatch="//",
        label="arm C impact band",
    )
    # Implementation shortfall, drawn faintly: it is the same executions with
    # horizon drift left in, and at small n the drift swamps everything.
    ax1.plot(sizes, [s[1] for s in stats["shortfall"]], marker="x", ls=":",
             color="#999", label="sliced, arrival-benchmarked (drift-contaminated)")
    ax1.set_xlabel("order size (BTC)")
    ax1.set_ylabel("execution cost (bps)")
    ax1.set_title("Cost vs size — drift-free\nshaded = inter-quartile range")
    ax1.axhline(0, lw=0.8, color="#888")
    ax1.legend(fontsize=8)
    ax1.grid(alpha=0.25)

    ax2.plot(sizes, [s[1] for s in stats["routed"]], marker="o",
             label="routed slippage", color=colors["routed"])
    ax2.plot(sizes, fee_med, marker="s", label="taker fee paid", color="#7b2cbf")
    ax2.set_yscale("log")
    ax2.set_xlabel("order size (BTC)")
    ax2.set_ylabel("bps (log scale)")
    ax2.set_title("Slippage vs fee\nthe gap is the whole story")
    ax2.legend(fontsize=8)
    ax2.grid(alpha=0.25, which="both")

    fig.suptitle(
        "BTC-USD execution cost on consolidated CB+KR depth — "
        "all figures are LOWER BOUNDS (displayed depth only)",
        fontsize=10,
    )
    fig.tight_layout()
    fig.savefig(args.out, dpi=150)
    print(f"wrote {args.out}")

    print(f"\n{'size':>8} {'naive':>9} {'routed':>9} {'sliced':>9} {'fee':>9} {'n':>5}")
    for i, s in enumerate(sizes):
        print(f"{s:8.3f} {stats['naive'][i][1]:9.2f} {stats['routed'][i][1]:9.2f} "
              f"{stats['sliced'][i][1]:9.2f} {fee_med[i]:9.2f} "
              f"{len(by_size[s]['routed']):5d}")


if __name__ == "__main__":
    main()

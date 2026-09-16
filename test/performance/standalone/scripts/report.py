#!/usr/bin/env python3
"""Aggregate results/*.json (fortio) into a self-contained dashboard.html.

Groups runs by mode (baseline|minimal|crs, parsed from the filename prefix),
takes the MEDIAN across repeats per percentile (ARCHITECTURE.md §9.6), computes
WAF cost as a delta vs the baseline median, and emits one standalone HTML file
(no network, no build step) with charts, a data table, and a dark-mode toggle.

    python3 scripts/report.py [--results results] [--out dashboard.html]
"""
import argparse, glob, json, os, statistics, datetime

PCTS = ["p50", "p90", "p95", "p99", "p99.9"]
PCT_KEY = {"p50": 50.0, "p90": 90.0, "p95": 95.0, "p99": 99.0, "p99.9": 99.9}
MODE_ORDER = ["baseline", "minimal", "crs"]
MODE_LABEL = {"baseline": "Baseline (no WAF)", "minimal": "Minimal (~2 rules)", "crs": "CRS (~900 rules)"}


def parse_run(path):
    d = json.load(open(path))
    h = d.get("DurationHistogram", {})
    pct = {round(p["Percentile"], 1): p["Value"] * 1000.0 for p in h.get("Percentiles", [])}
    codes = d.get("RetCodes", {})
    total = sum(codes.values()) or 1
    ok = codes.get("200", 0)
    blocked = codes.get("403", 0)  # WAF block, not an error
    return {
        "pcts": {p: pct.get(PCT_KEY[p]) for p in PCTS},
        "qps": d.get("ActualQPS", 0.0),
        "err": 100.0 * (total - ok - blocked) / total,
        "requested_qps": d.get("RequestedQPS"),
        "conn": d.get("NumThreads"),
    }


def median(xs):
    xs = [x for x in xs if x is not None]
    return statistics.median(xs) if xs else None


def aggregate(results_dir):
    runs = {}
    for path in sorted(glob.glob(os.path.join(results_dir, "*.json"))):
        mode = os.path.basename(path).split("__", 1)[0]
        if mode not in MODE_ORDER:
            continue
        runs.setdefault(mode, []).append(parse_run(path))

    modes = []
    for mode in MODE_ORDER:
        rs = runs.get(mode)
        if not rs:
            continue
        modes.append({
            "mode": mode,
            "label": MODE_LABEL[mode],
            "runs": len(rs),
            "pcts": {p: median([r["pcts"][p] for r in rs]) for p in PCTS},
            "qps": median([r["qps"] for r in rs]),
            "err": median([r["err"] for r in rs]),
            "conn": next((r["conn"] for r in rs if r["conn"]), None),
            "requested_qps": next((r["requested_qps"] for r in rs if r["requested_qps"]), None),
        })

    base = next((m for m in modes if m["mode"] == "baseline"), None)
    for m in modes:
        if base and m["mode"] != "baseline":
            m["delta"] = {p: (m["pcts"][p] - base["pcts"][p])
                          if (m["pcts"][p] is not None and base["pcts"][p] is not None) else None
                          for p in PCTS}
        else:
            m["delta"] = {p: None for p in PCTS}
    return modes


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--results", default=os.path.join(os.path.dirname(__file__), "..", "results"))
    ap.add_argument("--out", default=os.path.join(os.path.dirname(__file__), "..", "dashboard.html"))
    args = ap.parse_args()

    modes = aggregate(args.results)
    if not modes:
        raise SystemExit(f"no results found in {args.results} — run scripts/suite.sh first")

    payload = {
        "generated": datetime.datetime.now().strftime("%Y-%m-%d %H:%M"),
        "percentiles": PCTS,
        "modes": modes,
    }
    tpl = open(os.path.join(os.path.dirname(__file__), "dashboard.tpl.html")).read()
    html = tpl.replace("/*__DATA__*/null", json.dumps(payload))
    out = os.path.abspath(args.out)
    open(out, "w").write(html)
    print(f">> wrote {out}  ({sum(m['runs'] for m in modes)} runs across {len(modes)} modes)")


if __name__ == "__main__":
    main()

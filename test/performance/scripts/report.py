#!/usr/bin/env python3
"""Aggregate Path A (in-cluster) results into a self-contained dashboard.html.

Groups fortio runs by TAG (parsed from `<scenario>__<tag>__<ts>.json`), treats
the `waf-off` tag as the B0 baseline, takes the MEDIAN across repeats per
percentile (ARCHITECTURE.md §9.6), and computes WAF cost as a delta vs B0.

If `collect.sh` was used, the matching `metrics__<tag>__<ts>.jsonl` files are
folded in as per-tag server-side SUMMARY tiles (RPS by outcome, active rule
count, plugin reloads in-window, cache p99, cache size, workqueue). The output
is one self-contained HTML file — the durable per-run artifact (§13): no live
Grafana/Prometheus needed to view it later or to compare runs across time.

    python3 scripts/report.py [--scenario NAME] [--results results] [--out dashboard.html]
"""
import argparse, glob, json, os, re, statistics, datetime, collections
try:
    import yaml as _yaml
except ImportError:
    _yaml = None

PCTS = ["p50", "p90", "p95", "p99", "p99.9"]
PCT_KEY = {"p50": 50.0, "p90": 90.0, "p95": 95.0, "p99": 99.0, "p99.9": 99.9}
BASELINE_TAG = "waf-off"


def median(xs):
    xs = [x for x in xs if x is not None]
    return statistics.median(xs) if xs else None


def mean(xs):
    xs = [x for x in xs if x is not None]
    return sum(xs) / len(xs) if xs else None


# ---- fortio JSON -----------------------------------------------------------
def parse_run(path):
    d = json.load(open(path))
    h = d.get("DurationHistogram", {})
    pct = {round(p["Percentile"], 1): p["Value"] * 1000.0 for p in h.get("Percentiles", [])}
    codes = d.get("RetCodes", {})
    total = sum(codes.values()) or 1
    ok = sum(v for k, v in codes.items() if str(k).startswith("2"))
    blocked = sum(v for k, v in codes.items() if str(k) == "403")  # WAF block, not an error
    return {
        "pcts": {p: pct.get(PCT_KEY[p]) for p in PCTS},
        "qps": d.get("ActualQPS", 0.0),
        "err": 100.0 * (total - ok - blocked) / total,
        "blocked": 100.0 * blocked / total,
        "requested_qps": d.get("RequestedQPS"),
        "conn": d.get("NumThreads"),
    }


# ---- collect.sh metrics jsonl ---------------------------------------------
def _series(resp):
    """Yield (labels, [float values]) from a Prometheus query_range response."""
    if not isinstance(resp, dict):
        return
    for s in resp.get("data", {}).get("result", []):
        vals = []
        for _, v in s.get("values", []):
            try:
                vals.append(float(v))
            except (TypeError, ValueError):
                pass
        yield s.get("metric", {}), vals


def parse_metrics(path):
    """Reduce a metrics__*.jsonl file to a compact server-side summary."""
    by_name = collections.defaultdict(list)  # metric name -> [(labels, vals)]
    for line in open(path):
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError:
            continue
        for labels, vals in _series(obj.get("result", {})):
            by_name[obj.get("metric", "")].append((labels, vals))

    def all_vals(name):
        return [v for _, vs in by_name.get(name, []) for v in vs]

    srv = {}
    # active rule count — the ruleset actually loaded (max over window)
    rc = all_vals("waf_active_rule_count")
    srv["rule_count"] = int(max(rc)) if rc else None
    # RPS by outcome — mean over window per outcome label
    rps = {}
    for labels, vals in by_name.get("waf_rps_by_outcome", []):
        m = mean(vals)
        if m is not None:
            rps[labels.get("outcome", "?")] = m
    srv["rps"] = rps or None
    # plugin reloads DURING the window (counter delta; should be 0 for a valid run)
    loads = by_name.get("waf_plugin_loads", [])
    if loads:
        per_ts = collections.defaultdict(float)
        for _, vals in loads:  # sum across status series is already done by the query
            if vals:
                srv["plugin_loads_delta"] = max(vals) - min(vals)
                break
    srv.setdefault("plugin_loads_delta", None)
    # cache poll p99 (seconds -> ms), cache size (bytes -> MB), workqueue, anomaly
    cp99 = all_vals("cache_latency_p99")
    srv["cache_p99_ms"] = max(cp99) * 1000.0 if cp99 else None
    cs = all_vals("cache_size_bytes")
    srv["cache_size_mb"] = max(cs) / 1e6 if cs else None
    wq = all_vals("workqueue_depth")
    srv["workqueue_max"] = max(wq) if wq else None
    an = all_vals("waf_anomaly_p95")
    srv["anomaly_p95"] = mean(an)
    return srv if any(v is not None for v in srv.values()) else None


# ---- aggregation -----------------------------------------------------------
def scenarios_in(results_dir):
    found = set()
    for path in glob.glob(os.path.join(results_dir, "*.json")):
        parts = os.path.basename(path).split("__")
        if len(parts) >= 3:
            found.add(parts[0])
    return sorted(found)


def aggregate(results_dir, scenario):
    fruns = collections.defaultdict(list)  # tag -> [run]
    for path in sorted(glob.glob(os.path.join(results_dir, f"{scenario}__*.json"))):
        parts = os.path.basename(path).split("__")
        if len(parts) < 3:
            continue
        fruns[parts[1]].append(parse_run(path))

    mruns = collections.defaultdict(list)  # tag -> [server summary]
    for path in sorted(glob.glob(os.path.join(results_dir, "metrics__*.jsonl"))):
        parts = os.path.basename(path).split("__")
        if len(parts) < 3:
            continue
        s = parse_metrics(path)
        if s:
            mruns[parts[1]].append(s)

    # order: baseline first, then by ruleset complexity (minimal < medium < crs), unknown last
    _RULESET_RANK = {"off": 0, "minimal": 1, "medium": 2, "crs": 3}
    def _tag_rank(t):
        suffix = t[len("waf-"):] if t.startswith("waf-") else t
        return _RULESET_RANK.get(suffix, 99)
    tags = sorted(fruns, key=lambda t: (t != BASELINE_TAG, _tag_rank(t), t))
    modes = []
    for tag in tags:
        rs = fruns[tag]
        srv_list = mruns.get(tag)
        modes.append({
            "mode": tag,
            "label": ("WAF off (B0)" if tag == BASELINE_TAG else tag),
            "runs": len(rs),
            "pcts": {p: median([r["pcts"][p] for r in rs]) for p in PCTS},
            "qps": median([r["qps"] for r in rs]),
            "err": median([r["err"] for r in rs]),
            "conn": next((r["conn"] for r in rs if r["conn"]), None),
            "requested_qps": next((r["requested_qps"] for r in rs if r["requested_qps"]), None),
            "server": _median_server(srv_list) if srv_list else None,
        })

    base = next((m for m in modes if m["mode"] == BASELINE_TAG), None)
    for m in modes:
        if base and m["mode"] != BASELINE_TAG:
            m["delta"] = {p: (m["pcts"][p] - base["pcts"][p])
                          if (m["pcts"][p] is not None and base["pcts"][p] is not None) else None
                          for p in PCTS}
        else:
            m["delta"] = {p: None for p in PCTS}
    return modes


def _median_server(srv_list):
    out = {}
    keys = set().union(*[s.keys() for s in srv_list])
    for k in keys:
        if k == "rps":
            outcomes = set().union(*[(s.get("rps") or {}).keys() for s in srv_list])
            out["rps"] = {o: median([(s.get("rps") or {}).get(o) for s in srv_list]) for o in outcomes} or None
        else:
            out[k] = median([s.get(k) for s in srv_list])
    return out


def _parse_env_fragment(path):
    """Pull KEY="value" and HEADERS=( ... ) out of a sourced bash fragment.

    Deliberately not a shell: these files are flat assignments, and shelling out
    to source them would execute whatever a user put in a custom profile.
    """
    out, headers = {}, []
    try:
        text = open(path).read()
    except OSError:
        return None
    m = re.search(r"HEADERS=\((.*?)\)", text, re.S)
    if m:
        headers = re.findall(r'"([^"]*)"', m.group(1))
    for line in text.splitlines():
        line = line.split("#", 1)[0].strip()
        km = re.match(r'^([A-Z_]+)=(.*)$', line)
        if km and km.group(1) != "HEADERS":
            out[km.group(1)] = km.group(2).strip().strip('"').strip("'")
    out["HEADERS"] = headers
    return out


def parse_scenario(scenarios_dir, name):
    """Load intensity knobs for the scenario behind a result group."""
    env = _parse_env_fragment(os.path.join(scenarios_dir, f"{name}.env"))
    if env is None:
        return {"name": name, "found": False}
    return {
        "name": name, "found": True,
        "qps": env.get("QPS"), "duration": env.get("DURATION"), "conns": env.get("CONNS"),
    }


def parse_load_profile(profiles_dir, name):
    """Request shape for the load profile behind a result group."""
    env = _parse_env_fragment(os.path.join(profiles_dir, f"{name}.env"))
    if env is None:
        return {"name": name, "found": False}
    size = env.get("PAYLOAD_SIZE") or "0"
    method = env.get("METHOD") or ""
    has_body = bool(env.get("PAYLOAD_FILE") or env.get("PAYLOAD")) or size not in ("", "0")
    return {
        "name": name, "found": True,
        # fortio implies POST when a body is set and no method is given
        "method": method or ("POST" if has_body else "GET"),
        "method_implied": not method,
        "path": env.get("REQ_PATH") or "/",
        "content_type": env.get("CONTENT_TYPE") or "",
        "payload_file": env.get("PAYLOAD_FILE") or "",
        "payload_inline": bool(env.get("PAYLOAD")),
        "payload_size": int(size) if size.isdigit() else 0,
        "headers": env.get("HEADERS") or [],
        "expect_status": env.get("EXPECT_STATUS") or "200",
    }


def parse_rulesets(manifests_dir):
    """Read rulesets/*.yaml and return {ruleset_name: {name, rule_count, sources}}."""
    if _yaml is None:
        return {}
    rulesets_dir = os.path.join(manifests_dir, "rulesets")
    result = {}
    for path in sorted(glob.glob(os.path.join(rulesets_dir, "*.yaml"))):
        rs_name = os.path.basename(path).replace(".yaml", "")
        sources = []
        with open(path) as f:
            for doc in _yaml.safe_load_all(f):
                if not doc or doc.get("kind") != "RuleSource":
                    continue
                rules_text = (doc.get("spec") or {}).get("rules", "") or ""
                rule_count = len(re.findall(r"^SecRule\b|^SecAction\b", rules_text, re.MULTILINE))
                sources.append({
                    "name": doc["metadata"]["name"],
                    "rules": rules_text.strip(),
                    "rule_count": rule_count,
                })
        result[rs_name] = {
            "name": rs_name,
            "rule_count": sum(s["rule_count"] for s in sources),
            "sources": sources,
        }
    return result


def main():
    here = os.path.dirname(__file__)
    ap = argparse.ArgumentParser()
    ap.add_argument("--scenario", help="scenario name (default: the only one present)")
    ap.add_argument("--results", default=os.path.join(here, "..", "results"))
    ap.add_argument("--out", default=os.path.join(here, "..", "dashboard.html"))
    args = ap.parse_args()

    scns = scenarios_in(args.results)
    if not scns:
        raise SystemExit(f"no *.json results in {args.results} — run scripts/suite.sh first")
    scenario = args.scenario
    if not scenario:
        if len(scns) == 1:
            scenario = scns[0]
        else:
            raise SystemExit(f"multiple scenarios present {scns}; pass --scenario <name>")
    elif scenario not in scns:
        raise SystemExit(f"scenario '{scenario}' not found; present: {scns}")

    modes = aggregate(args.results, scenario)
    if not modes:
        raise SystemExit(f"no runs for scenario '{scenario}' in {args.results}")

    manifests_dir = os.path.join(here, "..", "manifests")

    # A result group is "<scenario>" or "<scenario>+<load-profile>", so the
    # configuration under test is recoverable from the group name alone.
    scn_name, _, prof_name = scenario.partition("+")
    scn_info = parse_scenario(os.path.join(here, "..", "scenarios"), scn_name)
    prof_info = (parse_load_profile(os.path.join(manifests_dir, "load-profiles"), prof_name)
                 if prof_name else None)

    raw_rulesets = parse_rulesets(manifests_dir)
    rulesets = []
    for m in modes:
        tag = m["mode"]
        if tag == BASELINE_TAG:
            rulesets.append({"name": tag, "rule_count": 0, "sources": []})
        else:
            rs_name = tag[len("waf-"):] if tag.startswith("waf-") else tag
            p = raw_rulesets.get(rs_name, {"name": tag, "rule_count": 0, "sources": []})
            rulesets.append({**p, "name": tag})  # tag ("waf-minimal") overrides file name

    payload = {
        "generated": datetime.datetime.now().strftime("%Y-%m-%d %H:%M"),
        "scenario": scenario,
        "percentiles": PCTS,
        "modes": modes,
        "has_server": any(m.get("server") for m in modes),
        "rulesets": rulesets,
        "scenario_cfg": scn_info,
        "profile_cfg": prof_info,
    }
    tpl = open(os.path.join(here, "report.tpl.html")).read()
    # CRS rule text contains literal "</script" (XSS signatures); embedding it
    # raw would close the <script> tag early and break the page. "<\/" is a
    # valid JSON escape, so the parsed strings are unchanged.
    blob = json.dumps(payload).replace("</", "<\\/")
    html = tpl.replace("/*__DATA__*/null", blob)
    out = os.path.abspath(args.out)
    open(out, "w").write(html)
    print(f">> wrote {out}  (scenario='{scenario}', {sum(m['runs'] for m in modes)} runs "
          f"across {len(modes)} tags, server-metrics={'yes' if payload['has_server'] else 'no'})")


if __name__ == "__main__":
    main()

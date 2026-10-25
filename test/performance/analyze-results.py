#!/usr/bin/env python3
"""
Analyze performance test results and generate comprehensive data for visualizations.
"""

import json
import glob
from pathlib import Path
from collections import defaultdict

def parse_result(filepath):
    """Extract all metrics from a fortio JSON result file."""
    with open(filepath) as f:
        data = json.load(f)

    hist = data.get('DurationHistogram', {})
    percentiles = {str(p['Percentile']): p['Value'] * 1000
                  for p in hist.get('Percentiles', [])}

    codes = data.get('RetCodes', {})
    total_requests = sum(codes.values()) or 1
    errors = sum(v for k, v in codes.items() if k != '200')
    error_rate = (errors / total_requests) * 100 if total_requests > 0 else 0

    return {
        'count': hist.get('Count', 0),
        'p50': percentiles.get('50', 0),
        'p75': percentiles.get('75', 0),
        'p90': percentiles.get('90', 0),
        'p95': percentiles.get('95', 0),
        'p99': percentiles.get('99', 0),
        'p99_9': percentiles.get('99.9', 0),
        'avg': hist.get('Avg', 0) * 1000,
        'stddev': hist.get('StdDev', 0) * 1000,
        'min': hist.get('Min', 0) * 1000,
        'max': hist.get('Max', 0) * 1000,
        'actual_qps': data.get('ActualQPS', 0),
        'requested_qps': float(data.get('RequestedQPS', 0)),
        'codes': codes,
        'error_rate': error_rate,
        'errors': errors
    }

def find_latest_result(results_dir, profile, config):
    """Find the most recent valid result file."""
    pattern = f"{results_dir}/fixed-500qps+{profile}__waf-{config}__*.json"
    files = sorted(glob.glob(pattern), reverse=True)

    for filepath in files:
        try:
            # Skip empty files
            if Path(filepath).stat().st_size == 0:
                continue
            parse_result(filepath)
            return filepath
        except:
            continue
    return None

def main():
    results_dir = Path(__file__).parent / 'results'

    profiles = ['baseline-get', 'args-heavy', 'headers-heavy']
    configs = ['off', 'minimal', 'medium', 'crs']

    # Ruleset metadata
    ruleset_info = {
        'off': {'rules': 0, 'name': 'No WAF'},
        'minimal': {'rules': 2, 'name': 'Minimal (2 rules)'},
        'medium': {'rules': 30, 'name': 'Medium (30 rules)'},
        'crs': {'rules': 563, 'name': 'OWASP CRS (563 rules)'}
    }

    # Load all results
    results = {}
    for profile in profiles:
        for config in configs:
            filepath = find_latest_result(results_dir, profile, config)
            if filepath:
                results[(profile, config)] = parse_result(filepath)

    print("="*80)
    print("PERFORMANCE TEST ANALYSIS")
    print("="*80)
    print()

    # 1. LATENCY DISTRIBUTION ANALYSIS
    print("1. LATENCY DISTRIBUTION (Full Percentile View)")
    print("-" * 80)
    print(f"{'Config':<15} {'p50':>8} {'p75':>8} {'p90':>8} {'p95':>8} {'p99':>8} {'p99.9':>8}")
    print("-" * 80)

    for profile in profiles:
        print(f"\n{profile.upper()}:")
        for config in configs:
            key = (profile, config)
            if key in results:
                r = results[key]
                print(f"  {config:<13} {r['p50']:>7.2f}ms {r['p75']:>7.2f}ms "
                      f"{r['p90']:>7.2f}ms {r['p95']:>7.2f}ms {r['p99']:>7.2f}ms {r['p99_9']:>7.2f}ms")

    # 2. THROUGHPUT ANALYSIS
    print("\n\n2. THROUGHPUT IMPACT")
    print("-" * 80)
    print(f"{'Profile':<20} {'Config':<15} {'Requested':>12} {'Actual':>12} {'% Achieved':>12}")
    print("-" * 80)

    for profile in profiles:
        for config in configs:
            key = (profile, config)
            if key in results:
                r = results[key]
                pct = (r['actual_qps'] / r['requested_qps']) * 100 if r['requested_qps'] > 0 else 0
                print(f"{profile:<20} {config:<15} {r['requested_qps']:>10.0f}qps "
                      f"{r['actual_qps']:>10.1f}qps {pct:>10.1f}%")

    # 3. LATENCY CONSISTENCY (StdDev Analysis)
    print("\n\n3. LATENCY CONSISTENCY (Standard Deviation)")
    print("-" * 80)
    print(f"{'Profile':<20} {'Config':<15} {'Avg':>10} {'StdDev':>10} {'Coefficient of Variation':>25}")
    print("-" * 80)

    for profile in profiles:
        for config in configs:
            key = (profile, config)
            if key in results:
                r = results[key]
                cv = (r['stddev'] / r['avg'] * 100) if r['avg'] > 0 else 0
                print(f"{profile:<20} {config:<15} {r['avg']:>9.2f}ms {r['stddev']:>9.2f}ms {cv:>23.1f}%")

    # 4. TAIL LATENCY GAP (p99 - p50)
    print("\n\n4. TAIL LATENCY GAP (p99 - p50)")
    print("-" * 80)
    print(f"{'Profile':<20} {'Config':<15} {'p50':>10} {'p99':>10} {'Gap':>10} {'Gap %':>10}")
    print("-" * 80)

    for profile in profiles:
        for config in configs:
            key = (profile, config)
            if key in results:
                r = results[key]
                gap = r['p99'] - r['p50']
                gap_pct = (gap / r['p50'] * 100) if r['p50'] > 0 else 0
                print(f"{profile:<20} {config:<15} {r['p50']:>9.2f}ms {r['p99']:>9.2f}ms "
                      f"{gap:>9.2f}ms {gap_pct:>9.1f}%")

    # 5. RULES EFFICIENCY (Latency per rule)
    print("\n\n5. RULES EFFICIENCY (Overhead per Rule)")
    print("-" * 80)
    print(f"{'Profile':<20} {'Config':<15} {'# Rules':>10} {'Overhead':>12} {'ms/rule':>12}")
    print("-" * 80)

    for profile in profiles:
        baseline_p50 = results.get((profile, 'off'), {}).get('p50', 0)
        for config in ['minimal', 'medium', 'crs']:
            key = (profile, config)
            if key in results and baseline_p50 > 0:
                r = results[key]
                overhead = r['p50'] - baseline_p50
                rules = ruleset_info[config]['rules']
                per_rule = overhead / rules if rules > 0 else 0
                print(f"{profile:<20} {config:<15} {rules:>10} {overhead:>11.2f}ms {per_rule:>11.4f}ms")

    # 6. ERROR ANALYSIS
    print("\n\n6. ERROR ANALYSIS")
    print("-" * 80)
    print(f"{'Profile':<20} {'Config':<15} {'Total Req':>12} {'Errors':>12} {'Error %':>12}")
    print("-" * 80)

    for profile in profiles:
        for config in configs:
            key = (profile, config)
            if key in results:
                r = results[key]
                print(f"{profile:<20} {config:<15} {r['count']:>12} "
                      f"{r['errors']:>12} {r['error_rate']:>11.2f}%")

    # 7. REQUEST COMPLEXITY IMPACT
    print("\n\n7. REQUEST COMPLEXITY IMPACT (How each ruleset scales)")
    print("-" * 80)
    print("Overhead increase from baseline-get → args-heavy → headers-heavy\n")

    for config in configs:
        print(f"{config.upper()} ({ruleset_info[config]['name']}):")
        baseline_lat = results.get(('baseline-get', config), {}).get('p50', 0)
        args_lat = results.get(('args-heavy', config), {}).get('p50', 0)
        headers_lat = results.get(('headers-heavy', config), {}).get('p50', 0)

        if baseline_lat > 0:
            args_increase = ((args_lat - baseline_lat) / baseline_lat * 100) if args_lat > 0 else 0
            headers_increase = ((headers_lat - baseline_lat) / baseline_lat * 100) if headers_lat > 0 else 0

            print(f"  baseline-get:   {baseline_lat:>7.2f}ms (baseline)")
            print(f"  args-heavy:     {args_lat:>7.2f}ms ({args_increase:>+6.1f}%)")
            print(f"  headers-heavy:  {headers_lat:>7.2f}ms ({headers_increase:>+6.1f}%)")
        print()

    # 8. VISUALIZATION RECOMMENDATIONS
    print("\n" + "="*80)
    print("RECOMMENDED VISUALIZATIONS")
    print("="*80)
    print("""
1. **Latency Distribution Chart** (Box plot or violin plot)
   - X-axis: WAF Config
   - Y-axis: Latency (ms)
   - Show p50, p75, p90, p95, p99 as box plot
   - Separate chart per profile OR grouped bars
   - Reveals tail latency behavior

2. **Throughput Impact Chart** (Bar chart)
   - X-axis: Profile + Config combinations
   - Y-axis: Actual QPS achieved
   - Target line at 500 QPS
   - Shows if WAF causes throughput degradation

3. **Latency Consistency Chart** (Scatter or bar)
   - X-axis: Configuration
   - Y-axis: Coefficient of Variation (StdDev/Avg %)
   - Lower is better (more consistent)
   - Color-code by profile

4. **Tail Latency Gap Chart** (Bar chart)
   - X-axis: Profile + Config
   - Y-axis: p99 - p50 gap (ms)
   - Shows consistency vs inconsistency
   - Large gap = unpredictable performance

5. **Rules Efficiency Chart** (Scatter plot)
   - X-axis: Number of rules
   - Y-axis: ms per rule
   - Each point is a (profile, config) combination
   - Shows diminishing returns or constant overhead

6. **Heatmap** (2D color matrix)
   - Rows: Load Profiles
   - Columns: WAF Configs
   - Color: Latency (p95)
   - Quick visual pattern recognition

7. **Request Complexity Scaling** (Line chart)
   - X-axis: Request complexity (baseline → args → headers)
   - Y-axis: Latency (p50)
   - One line per ruleset
   - Shows how each ruleset scales with request complexity

8. **Percentile Comparison Table**
   - Rows: All profile+config combinations
   - Columns: p50, p75, p90, p95, p99, p99.9
   - Sortable
   - Exportable

9. **Cost-Benefit Scatter**
   - X-axis: Security (number of rules)
   - Y-axis: Performance (p95 latency)
   - Each point labeled
   - Shows security/performance tradeoff

10. **Error Rate Chart** (If errors exist)
    - Bar chart showing error % per config
    """)

if __name__ == '__main__':
    main()

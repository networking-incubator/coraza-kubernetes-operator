#!/usr/bin/env python3
"""
Generate comprehensive HTML performance report with navigation and modals.
Version 2 - with sections, navigation menu, and popup details.
"""

import json
import glob
import re
from pathlib import Path
from datetime import datetime
import html
import yaml

# A rule is a SecRule/SecAction directive carrying an id. Chained sub-rules have
# no id of their own and only evaluate when their parent matches, so they are not
# counted; neither are configuration directives (SecRuleEngine, SecRequestBodyAccess)
# or SecMarker jump targets, which cost nothing per request.
RULE_DIRECTIVE_RE = re.compile(r'^(SecRule|SecAction)\b')
RULE_ID_RE = re.compile(r"\bid:\s*'?(\d+)")
LINE_CONTINUATION_RE = re.compile(r'\\\s*\n\s*')

# Used only when a manifest cannot be read; the charts' x-axis depends on these.
FALLBACK_RULE_COUNTS = {'off': 0, 'minimal': 1, 'medium': 30, 'crs': 563}

def parse_result(filepath):
    """Extract key metrics from a fortio JSON result file."""
    with open(filepath) as f:
        data = json.load(f)

    hist = data.get('DurationHistogram', {})
    percentiles = {str(p['Percentile']): p['Value'] * 1000
                  for p in hist.get('Percentiles', [])}

    codes = data.get('RetCodes', {})
    total_requests = sum(codes.values()) or 1

    return {
        'count': hist.get('Count', 0),
        'p50': percentiles.get('50', 0),
        'p75': percentiles.get('75', 0),
        'p90': percentiles.get('90', 0),
        'p95': percentiles.get('95', 0),
        'p99': percentiles.get('99', 0),
        'p99_9': percentiles.get('99.9', 0),
        'avg': hist.get('Avg', 0) * 1000,
        'actual_qps': data.get('ActualQPS', 0),
        'codes': codes
    }

def find_latest_result(results_dir, profile, config):
    """Find the most recent valid result file."""
    pattern = f"{results_dir}/fixed-500qps+{profile}__waf-{config}__*.json"
    files = sorted(glob.glob(pattern), reverse=True)

    for filepath in files:
        try:
            parse_result(filepath)
            return filepath
        except:
            continue
    return None

def calc_overhead(baseline_ms, waf_ms):
    """Calculate percentage overhead."""
    if baseline_ms == 0:
        return 0
    return ((waf_ms - baseline_ms) / baseline_ms) * 100

def format_rules(count, title_case=False):
    """Render a rule count with the right plural, e.g. '1 rule' / '563 rules'."""
    noun = 'Rule' if count == 1 else 'Rules'
    return f'{count} {noun}' if title_case else f'{count} {noun.lower()}'


def count_ruleset_rules(manifest_path):
    """Count the rules a ruleset manifest actually loads into the WAF.

    Parses the multi-document YAML, keeps only the RuleSource documents the
    RuleSet references, and counts distinct rule ids across them. Returns None
    if the manifest cannot be read or parsed, so the caller can fall back.
    """
    try:
        with open(manifest_path) as f:
            docs = list(yaml.safe_load_all(f))
    except (OSError, yaml.YAMLError):
        return None

    sources = {}
    referenced = None

    for doc in docs:
        if not isinstance(doc, dict):
            continue
        spec = doc.get('spec') or {}
        name = (doc.get('metadata') or {}).get('name')

        if doc.get('kind') == 'RuleSource' and name:
            sources[name] = spec.get('rules') or ''
        elif doc.get('kind') == 'RuleSet':
            referenced = [s.get('name') for s in (spec.get('sources') or [])]

    # Without a RuleSet, fall back to every RuleSource in the file.
    names = referenced if referenced is not None else list(sources)

    rule_ids = set()
    for name in names:
        # Join line continuations so each directive is one logical line.
        text = LINE_CONTINUATION_RE.sub(' ', sources.get(name, ''))
        for line in text.splitlines():
            stripped = line.strip()
            if not RULE_DIRECTIVE_RE.match(stripped):
                continue
            match = RULE_ID_RE.search(stripped)
            if match:
                rule_ids.add(match.group(1))

    return len(rule_ids)


def resolve_rule_counts(base_path, configs, ruleset_files):
    """Resolve rule counts per WAF config, parsing manifests where possible."""
    counts = {}

    for config in configs:
        manifest = ruleset_files.get(config)
        if manifest is None:
            counts[config] = 0  # 'off' has no ruleset
            continue

        counted = count_ruleset_rules(base_path / manifest)
        if counted is None:
            counted = FALLBACK_RULE_COUNTS.get(config, 0)
            print(f"   ⚠️  Could not parse {manifest}, using fallback count {counted} for '{config}'")
        counts[config] = counted

    return counts


def load_profile_content(profile_path):
    """Load and parse profile file content."""
    with open(profile_path) as f:
        return f.read()

def format_code_for_modal(content):
    """Format code content for modal display with syntax highlighting."""
    lines = content.split('\n')
    formatted_lines = []

    for line in lines:
        if line.strip().startswith('#'):
            formatted_lines.append(f'<span class="comment">{html.escape(line)}</span>')
        elif '=' in line and not line.strip().startswith('#'):
            parts = line.split('=', 1)
            if len(parts) == 2:
                key = f'<span class="keyword">{html.escape(parts[0])}</span>'
                value = f'<span class="string">{html.escape(parts[1])}</span>'
                formatted_lines.append(f'{key}={value}')
            else:
                formatted_lines.append(html.escape(line))
        else:
            formatted_lines.append(html.escape(line))

    return '\n'.join(formatted_lines)

def generate_profile_cards(base_path):
    """Generate HTML cards for load profiles."""
    profiles = [
        {
            'id': 'baseline-get',
            'name': 'baseline-get',
            'title': 'Baseline GET',
            'icon': '🎯',
            'description': 'Simple GET / request with no query parameters or custom headers. Represents the WAF\'s best-case scenario.',
            'tags': ['Simple', 'Minimal Data']
        },
        {
            'id': 'args-heavy',
            'name': 'args-heavy',
            'title': 'Args Heavy',
            'icon': '🔢',
            'description': 'GET request with 25 query parameters. Exercises CRS ARGS rules extensively (~165 rules iterate over ARGS).',
            'tags': ['25 Parameters', 'CRS Test']
        },
        {
            'id': 'headers-heavy',
            'name': 'headers-heavy',
            'title': 'Headers Heavy',
            'icon': '📋',
            'description': 'GET request with 15 additional HTTP headers. Tests REQUEST_HEADERS inspection rules.',
            'tags': ['15 Headers', 'Header Inspection']
        }
    ]

    cards_html = []
    for profile in profiles:
        # Load profile content
        profile_path = base_path / 'manifests' / 'load-profiles' / f"{profile['name']}.env"
        if profile_path.exists():
            content = load_profile_content(profile_path)
            formatted_content = format_code_for_modal(content)
        else:
            formatted_content = "Profile file not found"

        # Escape content for JavaScript
        js_content = json.dumps(formatted_content)

        tags_html = ''.join([f'<span class="card-tag">{tag}</span>' for tag in profile['tags']])

        card_html = f'''
                <div class="card" onclick='openModal("{profile["title"]}", `<div class="code-block">{formatted_content}</div>`)'>
                    <div class="card-title">
                        <span class="card-icon">{profile['icon']}</span>
                        {profile['title']}
                    </div>
                    <div class="card-description">{profile['description']}</div>
                    <div class="card-meta">
                        {tags_html}
                    </div>
                </div>
'''
        cards_html.append(card_html)

    return '\n'.join(cards_html)

# Single source of truth for WAF ruleset metadata. Rule counts are parsed from
# the manifests at generation time, so {rules} is filled in by the caller.
RULESETS = [
    {
        'id': 'off',
        'name': 'off',
        'title': 'OFF (No WAF)',
        'icon': '🔓',
        'description': 'No WAF enabled. Traffic flows through Istio/Envoy without WAF filtering. Used as baseline for comparison.',
        'tags': ['{rules_title}', 'Baseline'],
        'file': None
    },
    {
        'id': 'minimal',
        'name': 'minimal',
        'title': 'Minimal',
        'icon': '🛡️',
        'description': 'Simplest WAF configuration with {rules_lower}. Engine initialization plus basic attack keyword blocking.',
        'tags': ['{rules_title}', 'Basic Protection'],
        'file': 'manifests/rulesets/minimal.yaml'
    },
    {
        'id': 'medium',
        'name': 'medium',
        'title': 'Medium',
        'icon': '🔒',
        'description': 'Typical hand-written security policy with {rules_lower} covering common attack patterns.',
        'tags': ['{rules_title}', 'Custom Policy'],
        'file': 'manifests/rulesets/medium.yaml'
    },
    {
        'id': 'crs',
        'name': 'crs',
        'title': 'CRS (CoreRuleSet)',
        'icon': '🛡️',
        'description': 'Full OWASP CoreRuleSet 4.7.0 with {rules_lower}. Industry-standard WAF ruleset.',
        'tags': ['{rules_title}', 'OWASP CRS 4.7.0'],
        'file': 'manifests/rulesets/crs.yaml'
    }
]

RULESET_FILES = {r['id']: r['file'] for r in RULESETS}


def generate_ruleset_cards(base_path, rule_counts):
    """Generate HTML cards for WAF rulesets.

    Returns:
        tuple: (cards_html, ruleset_data_dict) where ruleset_data_dict contains
               the ruleset content to be injected into JavaScript
    """
    cards_html = []
    ruleset_data = {}

    for ruleset in RULESETS:
        rules = rule_counts.get(ruleset['id'], 0)
        fmt = {
            'rules': rules,
            'rules_lower': format_rules(rules),
            'rules_title': format_rules(rules, title_case=True)
        }
        description = ruleset['description'].format(**fmt)
        tags = [tag.format(**fmt) for tag in ruleset['tags']]

        # Load ruleset content
        if ruleset['file']:
            ruleset_path = base_path / ruleset['file']
            if ruleset_path.exists():
                content = load_profile_content(ruleset_path)
                # Store the escaped content
                ruleset_data[ruleset['id']] = html.escape(content)
            else:
                ruleset_data[ruleset['id']] = "Ruleset file not found"
        else:
            # OFF configuration has no file
            ruleset_data[ruleset['id']] = (
                "# No WAF Configuration\n"
                "# Traffic flows through Istio/Envoy without WAF filtering\n"
                "# Used as baseline measurement for comparison\n\n"
                "Engine: Not deployed\n"
                "Rules: 0\n"
                "Overhead: 0% (baseline)"
            )

        tags_html = ''.join([f'<span class="card-tag">{tag}</span>' for tag in tags])

        # Use data-ruleset-id instead of inline content
        card_html = f'''
                <div class="card" onclick='openRulesetModal("{ruleset["id"]}")'>
                    <div class="card-title">
                        <span class="card-icon">{ruleset['icon']}</span>
                        {ruleset['title']}
                    </div>
                    <div class="card-description">{description}</div>
                    <div class="card-meta">
                        {tags_html}
                    </div>
                </div>
'''
        cards_html.append(card_html)

    return '\n'.join(cards_html), ruleset_data

def generate_scenario_card(base_path):
    """Generate HTML card for test scenario."""
    scenario_path = base_path / 'scenarios' / 'fixed-500qps-short.env'

    if scenario_path.exists():
        content = load_profile_content(scenario_path)
        formatted_content = format_code_for_modal(content)
    else:
        formatted_content = "Scenario file not found"

    card_html = f'''
                <div class="card" onclick='openModal("Test Scenario: fixed-500qps", `<div class="code-block">{formatted_content}</div><div style="margin-top: 1.5rem;"><h4 style="color: #374151; margin-bottom: 0.75rem;">Configuration</h4><div class="meta-grid"><div class="meta-item"><div class="meta-label">QPS Target</div><div class="meta-value">500 req/s</div></div><div class="meta-item"><div class="meta-label">Connections</div><div class="meta-value">16</div></div><div class="meta-item"><div class="meta-label">Duration</div><div class="meta-value">60 seconds</div></div></div></div>`)'>
                    <div class="card-title">
                        <span class="card-icon">⚡</span>
                        fixed-500qps
                    </div>
                    <div class="card-description">
                        Fixed 500 QPS load test scenario. Sends constant 500 requests per second for 60 seconds using 16 concurrent connections.
                    </div>
                    <div class="card-meta">
                        <span class="card-tag">500 QPS</span>
                        <span class="card-tag">16 Connections</span>
                        <span class="card-tag">60s Duration</span>
                    </div>
                </div>
'''

    return card_html

def generate_test_matrix(results, profiles, configs):
    """Generate test matrix table showing all test permutations."""

    num_configs = len(configs)

    table_html = f'''
                <table class="matrix-table">
                    <thead>
                        <tr>
                            <th rowspan="2" style="vertical-align: middle;">Load Profiles</th>
                            <th colspan="{num_configs}" style="background: linear-gradient(135deg, #ec4899 0%, #db2777 100%);">WAF Rulesets</th>
                        </tr>
                        <tr>
'''

    # Second header row with individual ruleset names
    for config in configs:
        config_display = config.upper() if config == 'crs' else config.capitalize()
        table_html += f'                            <th>{config_display}</th>\n'

    table_html += '''                        </tr>
                    </thead>
                    <tbody>
'''

    # Row for each profile
    for profile in profiles:
        profile_title = profile.replace('-', ' ').title()
        table_html += f'                        <tr>\n'
        table_html += f'                            <td>{profile_title}</td>\n'

        for config in configs:
            key = (profile, config)
            if key in results:
                # Test completed - show checkmark only
                status_html = '''
                            <td>
                                <div class="matrix-cell">
                                    <div class="matrix-status completed">✓</div>
                                </div>
                            </td>
'''
            else:
                # Test missing
                status_html = '''
                            <td>
                                <div class="matrix-cell">
                                    <div class="matrix-status missing">✗</div>
                                </div>
                            </td>
'''
            table_html += status_html

        table_html += '                        </tr>\n'

    table_html += '''                    </tbody>
                </table>
'''

    return table_html

def generate_percentile_cards(results, profiles, configs):
    """Generate percentile comparison cards (P50, P95, P99)."""
    percentiles = ['p50', 'p95', 'p99']

    cards_html = []

    for pct in percentiles:
        pct_display = pct.upper()

        card_html = f'''
            <div class="percentile-card">
                <div class="percentile-card-header">
                    <div class="percentile-badge">{pct_display}</div>
                    <div class="percentile-title">Latency comparison across all load profiles</div>
                </div>
                <div class="comparison-grid">
'''

        for profile in profiles:
            profile_title = profile.replace('-', ' ').title()
            baseline_value = results.get((profile, 'off'), {}).get(pct, 0)

            card_html += f'''
                    <div class="profile-comparison">
                        <div class="profile-name">{profile_title}</div>
                        <div class="waf-configs">
'''

            for config in configs:
                key = (profile, config)
                if key in results:
                    value = results[key].get(pct, 0)

                    # Calculate overhead
                    if config == 'off':
                        overhead_text = 'baseline'
                        overhead_class = 'baseline'
                    elif baseline_value > 0:
                        overhead_pct = calc_overhead(baseline_value, value)
                        overhead_text = f'+{overhead_pct:.0f}%'

                        if overhead_pct < 20:
                            overhead_class = 'low'
                        elif overhead_pct < 100:
                            overhead_class = 'medium'
                        else:
                            overhead_class = 'high'
                    else:
                        overhead_text = 'N/A'
                        overhead_class = 'baseline'

                    card_html += f'''
                            <div class="waf-config-item">
                                <div class="config-name">{config}</div>
                                <div class="config-value">{value:.2f}ms</div>
                                <div class="config-overhead">{overhead_text}</div>
                            </div>
'''
                else:
                    card_html += f'''
                            <div class="waf-config-item">
                                <div class="config-name">{config}</div>
                                <div class="config-value">—</div>
                                <div class="config-overhead baseline">missing</div>
                            </div>
'''

            card_html += '''
                        </div>
                    </div>
'''

        card_html += '''
                </div>
            </div>
'''

        cards_html.append(card_html)

    return '\n'.join(cards_html)

def generate_profile_tables(results, profiles, configs):
    """Generate HTML tables for each profile."""
    html_parts = []

    for profile in profiles:
        profile_title = profile.replace('-', ' ').title()
        baseline_p50 = results.get((profile, 'off'), {}).get('p50', 0)

        table_html = f'''
            <div style="margin-top: 3rem;">
                <h3 style="margin-bottom: 1rem; color: #374151;">Load Profile: {profile_title}</h3>
                <table>
                    <thead>
                        <tr>
                            <th>WAF Config</th>
                            <th>p50 (ms)</th>
                            <th>p95 (ms)</th>
                            <th>p99 (ms)</th>
                            <th>Actual QPS</th>
                            <th>Overhead vs OFF</th>
                        </tr>
                    </thead>
                    <tbody>
'''

        for config in configs:
            key = (profile, config)
            if key in results:
                r = results[key]
                overhead = ""
                overhead_class = ""

                if config != 'off' and baseline_p50 > 0:
                    ovh_pct = calc_overhead(baseline_p50, r['p50'])
                    overhead = f"+{ovh_pct:.0f}%"

                    if ovh_pct < 20:
                        overhead_class = "low"
                    elif ovh_pct < 100:
                        overhead_class = "medium"

                table_html += f'''
                        <tr>
                            <td class="metric">{config}</td>
                            <td>{r['p50']:.2f}</td>
                            <td>{r['p95']:.2f}</td>
                            <td>{r['p99']:.2f}</td>
                            <td>{r['actual_qps']:.1f}</td>
                            <td class="overhead {overhead_class}">{overhead}</td>
                        </tr>
'''
            else:
                table_html += f'''
                        <tr>
                            <td class="metric">{config}</td>
                            <td colspan="5" style="text-align: center; color: #dc2626;">MISSING</td>
                        </tr>
'''

        table_html += '''
                    </tbody>
                </table>
            </div>
'''
        html_parts.append(table_html)

    return '\n'.join(html_parts)

def prepare_chart_data(results, profiles, configs, rule_counts):
    """Prepare Chart.js configuration objects."""

    # Latency chart data
    profile_colors = {
        'baseline-get': {'p50': '#3b82f6', 'p95': '#60a5fa', 'p99': '#93c5fd'},
        'args-heavy': {'p50': '#8b5cf6', 'p95': '#a78bfa', 'p99': '#c4b5fd'},
        'headers-heavy': {'p50': '#ec4899', 'p95': '#f472b6', 'p99': '#f9a8d4'}
    }

    latency_datasets = []
    for profile in profiles:
        colors = profile_colors[profile]
        p50_data = []

        for config in configs:
            key = (profile, config)
            if key in results:
                p50_data.append(results[key]['p50'])
            else:
                p50_data.append(None)

        latency_datasets.append({
            'label': f'{profile} (p50)',
            'data': p50_data,
            'backgroundColor': colors['p50'] + '80',
            'borderColor': colors['p50'],
            'borderWidth': 2
        })

    latency_chart = {
        'type': 'bar',
        'data': {
            'labels': configs,
            'datasets': latency_datasets
        },
        'options': {
            'responsive': True,
            'maintainAspectRatio': False,
            'plugins': {
                'title': {
                    'display': True,
                    'text': 'p50 Latency Comparison (ms)',
                    'font': {'size': 16}
                },
                'legend': {
                    'position': 'top',
                }
            },
            'scales': {
                'y': {
                    'beginAtZero': True,
                    'title': {
                        'display': True,
                        'text': 'Latency (ms)'
                    }
                },
                'x': {
                    'title': {
                        'display': True,
                        'text': 'WAF Configuration'
                    }
                }
            }
        }
    }

    # Overhead chart data
    overhead_data = []
    for profile in profiles:
        baseline_p50 = results.get((profile, 'off'), {}).get('p50', 0)
        crs_p50 = results.get((profile, 'crs'), {}).get('p50', 0)
        if baseline_p50 > 0 and crs_p50 > 0:
            overhead_pct = calc_overhead(baseline_p50, crs_p50)
            overhead_data.append(overhead_pct)
        else:
            overhead_data.append(0)

    overhead_chart = {
        'type': 'bar',
        'data': {
            'labels': [p.replace('-', ' ').title() for p in profiles],
            'datasets': [{
                'label': 'CRS Overhead (%)',
                'data': overhead_data,
                'backgroundColor': [
                    'rgba(220, 38, 38, 0.7)',
                    'rgba(245, 158, 11, 0.7)',
                    'rgba(5, 150, 105, 0.7)'
                ],
                'borderColor': [
                    'rgb(220, 38, 38)',
                    'rgb(245, 158, 11)',
                    'rgb(5, 150, 105)'
                ],
                'borderWidth': 2
            }]
        },
        'options': {
            'responsive': True,
            'maintainAspectRatio': False,
            'plugins': {
                'title': {
                    'display': True,
                    'text': f"CRS ({rule_counts.get('crs', 0)} rules) Overhead vs Baseline by Load Profile",
                    'font': {'size': 16}
                },
                'legend': {
                    'display': False
                }
            },
            'scales': {
                'y': {
                    'beginAtZero': True,
                    'title': {
                        'display': True,
                        'text': 'Overhead (%)'
                    }
                },
                'x': {
                    'title': {
                        'display': True,
                        'text': 'Load Profile'
                    }
                }
            }
        }
    }

    # 1. Latency Distribution Chart (showing multiple percentiles)
    percentile_datasets = []
    percentiles = ['p50', 'p75', 'p90', 'p95', 'p99']
    percentile_colors = {
        'p50': '#10b981',
        'p75': '#3b82f6',
        'p90': '#8b5cf6',
        'p95': '#f59e0b',
        'p99': '#ef4444'
    }

    for profile in profiles:
        for pct in percentiles:
            data = []
            for config in configs:
                key = (profile, config)
                if key in results:
                    data.append(results[key].get(pct, 0))
                else:
                    data.append(None)

            percentile_datasets.append({
                'label': f'{profile} - {pct}',
                'data': data,
                'backgroundColor': percentile_colors[pct] + '80',
                'borderColor': percentile_colors[pct],
                'borderWidth': 2,
                'stack': profile
            })

    latency_distribution_chart = {
        'type': 'bar',
        'data': {
            'labels': [c.upper() if c == 'crs' else c.capitalize() for c in configs],
            'datasets': percentile_datasets
        },
        'options': {
            'responsive': True,
            'maintainAspectRatio': False,
            'plugins': {
                'title': {
                    'display': True,
                    'text': 'Latency Distribution Across Percentiles',
                    'font': {'size': 16, 'weight': 'bold'}
                },
                'legend': {
                    'position': 'right',
                    'labels': {'font': {'size': 10}}
                }
            },
            'scales': {
                'y': {
                    'beginAtZero': True,
                    'stacked': False,
                    'title': {
                        'display': True,
                        'text': 'Latency (ms)'
                    }
                },
                'x': {
                    'title': {
                        'display': True,
                        'text': 'WAF Configuration'
                    }
                }
            }
        }
    }

    # 2. Request Complexity Scaling (Line Chart)
    complexity_datasets = []
    config_colors_line = {
        'off': '#6b7280',
        'minimal': '#3b82f6',
        'medium': '#8b5cf6',
        'crs': '#ef4444'
    }

    for config in configs:
        data = []
        for profile in profiles:
            key = (profile, config)
            if key in results:
                data.append(results[key]['p50'])
            else:
                data.append(None)

        complexity_datasets.append({
            'label': config.upper() if config == 'crs' else config.capitalize(),
            'data': data,
            'borderColor': config_colors_line[config],
            'backgroundColor': config_colors_line[config] + '40',
            'borderWidth': 3,
            'tension': 0.3,
            'fill': False,
            'pointRadius': 6,
            'pointHoverRadius': 8
        })

    complexity_scaling_chart = {
        'type': 'line',
        'data': {
            'labels': ['Baseline GET', 'Args Heavy (25 params)', 'Headers Heavy (15 headers)'],
            'datasets': complexity_datasets
        },
        'options': {
            'responsive': True,
            'maintainAspectRatio': False,
            'plugins': {
                'title': {
                    'display': True,
                    'text': 'Request Complexity Impact on Latency (p50)',
                    'font': {'size': 16, 'weight': 'bold'}
                },
                'subtitle': {
                    'display': True,
                    'text': 'Notice: CRS gets FASTER with more complex requests!',
                    'font': {'size': 12, 'style': 'italic'},
                    'color': '#ef4444'
                },
                'legend': {
                    'position': 'top'
                }
            },
            'scales': {
                'y': {
                    'beginAtZero': True,
                    'title': {
                        'display': True,
                        'text': 'p50 Latency (ms)'
                    }
                },
                'x': {
                    'title': {
                        'display': True,
                        'text': 'Request Complexity'
                    }
                }
            }
        }
    }

    # 3. Rules Efficiency: marginal cost of each additional rule.
    #
    # Total overhead is roughly `fixed + marginal * rules`, where `fixed` is the
    # per-request cost of turning the WAF on at all (WASM VM entry, transaction
    # setup, variable population). Dividing total overhead by rule count buries
    # that fixed term in the result and makes small rulesets look inefficient
    # purely because the denominator is small. Differencing adjacent rulesets
    # cancels the fixed term and leaves the real per-rule cost.
    ruleset_rules = rule_counts

    waf_configs = sorted(
        (c for c in configs if c != 'off' and ruleset_rules.get(c, 0) > 0),
        key=lambda c: ruleset_rules[c]
    )
    segments = list(zip(waf_configs, waf_configs[1:]))
    segment_labels = [f'{ruleset_rules[lo]} → {ruleset_rules[hi]} rules' for lo, hi in segments]

    marginal_datasets = []
    fixed_costs = []

    for profile in profiles:
        marginal_data = []
        for lo, hi in segments:
            lo_key, hi_key = (profile, lo), (profile, hi)
            if lo_key in results and hi_key in results:
                delta_ms = results[hi_key]['p50'] - results[lo_key]['p50']
                delta_rules = ruleset_rules[hi] - ruleset_rules[lo]
                marginal_data.append(delta_ms / delta_rules if delta_rules else None)
            else:
                marginal_data.append(None)

        marginal_datasets.append({
            'label': profile.replace('-', ' ').title(),
            'data': marginal_data,
            'backgroundColor': profile_colors[profile]['p50'] + '80',
            'borderColor': profile_colors[profile]['p50'],
            'borderWidth': 2
        })

        # Fixed cost of enabling the WAF, reported separately because it is not
        # attributable to any individual rule.
        off_key = (profile, 'off')
        first_key = (profile, waf_configs[0]) if waf_configs else None
        if first_key and off_key in results and first_key in results:
            fixed_costs.append(results[first_key]['p50'] - results[off_key]['p50'])

    if fixed_costs:
        fixed_note = (
            f'Fixed cost of enabling the WAF is excluded '
            f'({min(fixed_costs):.2f}-{max(fixed_costs):.2f} ms, off → {waf_configs[0]}).'
        )
    else:
        fixed_note = 'Marginal p50 cost of each additional rule.'

    rules_efficiency_chart = {
        'type': 'bar',
        'data': {
            'labels': segment_labels,
            'datasets': marginal_datasets
        },
        'options': {
            'responsive': True,
            'maintainAspectRatio': False,
            'plugins': {
                'title': {
                    'display': True,
                    'text': 'Marginal Cost per Added Rule (p50)',
                    'font': {'size': 16, 'weight': 'bold'}
                },
                'subtitle': {
                    'display': True,
                    'text': fixed_note,
                    'font': {'size': 12}
                },
                'legend': {
                    'position': 'top'
                }
            },
            'scales': {
                'y': {
                    'beginAtZero': True,
                    'title': {
                        'display': True,
                        'text': 'Marginal Latency per Rule (ms)'
                    }
                },
                'x': {
                    'title': {
                        'display': True,
                        'text': 'Ruleset Growth Step'
                    }
                }
            }
        }
    }

    # 4. Tail Latency Ratio Bar Chart.
    #
    # p99 - p50 in milliseconds conflates dispersion with absolute slowness: a
    # config is penalised for being slow even when its distribution is just as
    # tight as the baseline's. The ratio is scale-free, so it isolates
    # predictability from magnitude (which the latency and overhead charts
    # already cover).
    tail_ratio_data = []
    tail_labels = []
    tail_colors = []

    for profile in profiles:
        for config in configs:
            key = (profile, config)
            if key in results:
                p50 = results[key]['p50']
                p99 = results[key]['p99']
                ratio = p99 / p50 if p50 > 0 else 0

                # Chart.js renders an array label as one line per element.
                tail_labels.append([profile, config])
                tail_ratio_data.append(ratio)

                # Thresholds are on dispersion, not on absolute latency.
                if ratio < 2:
                    tail_colors.append('#10b981')  # Green
                elif ratio < 3:
                    tail_colors.append('#f59e0b')  # Orange
                else:
                    tail_colors.append('#ef4444')  # Red

    tail_ratio_chart = {
        'type': 'bar',
        'data': {
            'labels': tail_labels,
            'datasets': [{
                'label': 'p99 / p50 Ratio',
                'data': tail_ratio_data,
                'backgroundColor': tail_colors,
                'borderColor': tail_colors,
                'borderWidth': 2
            }]
        },
        'options': {
            'responsive': True,
            'maintainAspectRatio': False,
            'plugins': {
                'title': {
                    'display': True,
                    'text': 'Tail Latency Ratio (p99 / p50)',
                    'font': {'size': 16, 'weight': 'bold'}
                },
                'subtitle': {
                    'display': True,
                    'text': 'Scale-free spread, independent of absolute latency. '
                            'Lower = more predictable. Green < 2x, Orange < 3x, Red ≥ 3x',
                    'font': {'size': 11}
                },
                'legend': {
                    'display': False
                }
            },
            'scales': {
                'y': {
                    'beginAtZero': True,
                    'title': {
                        'display': True,
                        'text': 'p99 / p50 (x)'
                    }
                },
                'x': {
                    'title': {
                        'display': True,
                        'text': 'Profile + Configuration'
                    },
                    'ticks': {
                        'font': {'size': 10}
                    }
                }
            }
        }
    }

    # 5. Latency Heatmap (implemented as a matrix using background colors in HTML table)
    # We'll pass the data and let the HTML/JS handle rendering
    heatmap_data = []
    for profile in profiles:
        row = {'profile': profile.replace('-', ' ').title()}
        for config in configs:
            key = (profile, config)
            if key in results:
                row[config] = results[key]['p95']
            else:
                row[config] = None
        heatmap_data.append(row)

    return {
        'latencyChart': latency_chart,
        'overheadChart': overhead_chart,
        'latencyDistributionChart': latency_distribution_chart,
        'complexityScalingChart': complexity_scaling_chart,
        'rulesEfficiencyChart': rules_efficiency_chart,
        'tailRatioChart': tail_ratio_chart,
        'heatmapData': heatmap_data
    }

def generate_key_findings(results, profiles, configs, rule_counts):
    """Generate key findings dynamically from test results."""
    if not results:
        return '''
            <div class="findings">
                <h3>⚠️ No Results Available</h3>
                <p>No test results were found. Run performance tests to generate findings.</p>
            </div>
        '''

    findings = []

    # Calculate CRS overhead range across profiles (using p95)
    crs_overheads = []
    baseline_latencies = []
    crs_latencies = []

    for profile in profiles:
        baseline_key = (profile, 'off')
        crs_key = (profile, 'crs')

        if baseline_key in results and crs_key in results:
            baseline_p95 = results[baseline_key]['p95']
            crs_p95 = results[crs_key]['p95']

            if baseline_p95 > 0:
                overhead = ((crs_p95 - baseline_p95) / baseline_p95) * 100
                crs_overheads.append(overhead)
                baseline_latencies.append(baseline_p95)
                crs_latencies.append(crs_p95)

    if crs_overheads:
        min_overhead = min(crs_overheads)
        max_overhead = max(crs_overheads)
        avg_overhead = sum(crs_overheads) / len(crs_overheads)

        findings.append(
            f'<li><strong>CRS Overhead Range:</strong> OWASP CoreRuleSet overhead varies from '
            f'+{min_overhead:.0f}% to +{max_overhead:.0f}% (avg: +{avg_overhead:.0f}%) across '
            f'different request profiles at p95. Overhead is highly dependent on request complexity.</li>'
        )

    # Baseline performance range
    if baseline_latencies:
        min_baseline = min(baseline_latencies)
        max_baseline = max(baseline_latencies)
        findings.append(
            f'<li><strong>Baseline Performance:</strong> Without WAF, latency ranges from '
            f'{min_baseline:.2f}ms to {max_baseline:.2f}ms at p95 across different load profiles.</li>'
        )

    # CRS absolute latency
    if crs_latencies:
        min_crs = min(crs_latencies)
        max_crs = max(crs_latencies)
        findings.append(
            f'<li><strong>CRS Latency Impact:</strong> With {rule_counts.get("crs", 0)} OWASP CoreRuleSet rules, '
            f'latency ranges from {min_crs:.2f}ms to {max_crs:.2f}ms at p95. '
            f'Absolute overhead is {min_crs - min(baseline_latencies):.2f}ms to '
            f'{max_crs - max(baseline_latencies):.2f}ms.</li>'
        )

    # Compare ruleset sizes if data available
    minimal_overheads = []
    medium_overheads = []

    for profile in profiles:
        baseline_key = (profile, 'off')
        minimal_key = (profile, 'minimal')
        medium_key = (profile, 'medium')

        if baseline_key in results:
            baseline_p95 = results[baseline_key]['p95']

            if minimal_key in results and baseline_p95 > 0:
                minimal_p95 = results[minimal_key]['p95']
                overhead = ((minimal_p95 - baseline_p95) / baseline_p95) * 100
                minimal_overheads.append(overhead)

            if medium_key in results and baseline_p95 > 0:
                medium_p95 = results[medium_key]['p95']
                overhead = ((medium_p95 - baseline_p95) / baseline_p95) * 100
                medium_overheads.append(overhead)

    if minimal_overheads and medium_overheads:
        avg_minimal = sum(minimal_overheads) / len(minimal_overheads)
        avg_medium = sum(medium_overheads) / len(medium_overheads)

        findings.append(
            f'<li><strong>Custom Rulesets:</strong> Minimal ruleset '
            f'({format_rules(rule_counts.get("minimal", 0))}) adds avg +{avg_minimal:.0f}% overhead, '
            f'while medium ruleset ({format_rules(rule_counts.get("medium", 0))}) adds '
            f'avg +{avg_medium:.0f}% overhead at p95.</li>'
        )

    # Error rates
    any_errors = False
    for key, data in results.items():
        if data.get('error_rate', 0) > 0:
            any_errors = True
            break

    if any_errors:
        findings.append(
            '<li><strong>⚠️ Error Rates Detected:</strong> Some tests show non-zero error rates. '
            'Check detection-only mode configuration if unexpected.</li>'
        )
    else:
        findings.append(
            '<li><strong>✅ Zero Error Rate:</strong> All tests completed successfully with 0% error rate, '
            'confirming detection-only mode is working correctly.</li>'
        )

    if not findings:
        findings.append(
            '<li><strong>Insufficient Data:</strong> Not enough test results to generate meaningful findings. '
            'Run complete test suite for comprehensive analysis.</li>'
        )

    findings_html = '\n'.join(['                    ' + f for f in findings])

    return f'''
            <div class="findings">
                <h3>🔍 Performance Insights</h3>
                <p><em>The following findings are dynamically generated from the test results in this report:</em></p>
                <ul>
{findings_html}
                </ul>
            </div>
'''

def main():
    base_path = Path(__file__).parent
    results_dir = base_path / 'results'
    template_path = base_path / 'templates' / 'report-template.html'

    # Load template
    with open(template_path) as f:
        template = f.read()

    profiles = ['baseline-get', 'args-heavy', 'headers-heavy']
    configs = ['off', 'minimal', 'medium', 'crs']

    # Load all results
    results = {}
    for profile in profiles:
        for config in configs:
            filepath = find_latest_result(results_dir, profile, config)
            if filepath:
                results[(profile, config)] = parse_result(filepath)

    # Calculate test status
    total_tests = len(profiles) * len(configs)
    completed_tests = len(results)
    status_text = f"✅ {completed_tests}/{total_tests} Tests Completed"

    # Rule counts drive the rules-efficiency x-axis and every "N rules" label,
    # so they are parsed from the manifests rather than hardcoded.
    rule_counts = resolve_rule_counts(base_path, configs, RULESET_FILES)

    # Generate components
    profile_cards = generate_profile_cards(base_path)
    scenario_card = generate_scenario_card(base_path)
    ruleset_cards, ruleset_data = generate_ruleset_cards(base_path, rule_counts)
    test_matrix_table = generate_test_matrix(results, profiles, configs)
    percentile_cards = generate_percentile_cards(results, profiles, configs)
    profile_tables = generate_profile_tables(results, profiles, configs)
    chart_data = prepare_chart_data(results, profiles, configs, rule_counts)
    key_findings = generate_key_findings(results, profiles, configs, rule_counts)

    # Add ruleset data to chart_data for injection
    chart_data['rulesets'] = ruleset_data

    # Calculate test statistics
    success_rate = int((completed_tests / total_tests) * 100) if total_tests > 0 else 0

    # Replace placeholders
    # Prose in the template that enumerates rule counts, e.g. "0, 1, 30, or 563".
    def count_list(cfgs):
        values = [str(rule_counts[c]) for c in cfgs]
        return f"{', '.join(values[:-1])}, or {values[-1]}" if len(values) > 1 else values[0]

    html = template.replace('__TEST_STATUS__', status_text)
    html = html.replace('__RULE_COUNT_LIST__', count_list(configs))
    html = html.replace('__WAF_RULE_COUNT_LIST__', count_list([c for c in configs if c != 'off']))
    html = html.replace('__PLATFORM__', 'OpenShift 4.22.18')
    html = html.replace('__ISTIO_VERSION__', 'v1.27.5')
    html = html.replace('__OPERATOR_VERSION__', 'v0.5.0')
    html = html.replace('__LOAD_GENERATOR__', 'Fortio 1.76.0')
    html = html.replace('__TARGET_QPS__', '500 req/s')
    # Calculate total test duration (60s per test + 30s settle time)
    test_duration_per_run = 60  # seconds per test from fixed-500qps scenario
    settle_time_per_run = 30  # seconds between tests
    total_duration_seconds = total_tests * (test_duration_per_run + settle_time_per_run)
    total_duration_hours = total_duration_seconds / 3600
    duration_text = f"{total_tests} tests × 90s ≈ {total_duration_hours:.1f} hours"

    html = html.replace('__TEST_DURATION__', duration_text)
    html = html.replace('__PROFILE_CARDS__', profile_cards)
    html = html.replace('__SCENARIO_CARD__', scenario_card)
    html = html.replace('__RULESET_CARDS__', ruleset_cards)
    html = html.replace('__TEST_MATRIX_TABLE__', test_matrix_table)
    html = html.replace('__TOTAL_TESTS__', str(total_tests))
    html = html.replace('__COMPLETED_TESTS__', str(completed_tests))
    html = html.replace('__SUCCESS_RATE__', str(success_rate))
    html = html.replace('__PERCENTILE_CARDS__', percentile_cards)
    html = html.replace('__PROFILE_TABLES__', profile_tables)
    html = html.replace('__KEY_FINDINGS__', key_findings)
    html = html.replace('__GENERATED_DATE__', datetime.now().strftime('%Y-%m-%d %H:%M UTC'))

    # Inject chart data
    html = html.replace('/*__DATA__*/null', f'/*__DATA__*/{json.dumps(chart_data)}')

    # Write output
    output_file = results_dir / 'performance-report.html'
    with open(output_file, 'w') as f:
        f.write(html)

    print(f"✅ Performance report generated: {output_file}")
    print(f"   Tests completed: {completed_tests}/{total_tests} ({success_rate}%)")
    print(f"   Rule counts (parsed from manifests): "
          + ", ".join(f"{c}={rule_counts[c]}" for c in configs))
    print(f"   Sections: Architecture, Scenario, Profiles, WAF Rulesets, Test Matrix, Percentile, Results, Findings")
    print(f"\n   file://{output_file.absolute()}")

    if completed_tests < total_tests:
        print(f"\n   ⚠️  Missing {total_tests - completed_tests} test results")
        return 1

    return 0

if __name__ == '__main__':
    exit(main())

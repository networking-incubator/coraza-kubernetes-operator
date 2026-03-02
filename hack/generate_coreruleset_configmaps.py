#!/usr/bin/env python3
# pylint: disable=missing-function-docstring,missing-module-docstring
"""
Generate Kubernetes ConfigMaps from OWASP CoreRuleSet rules.

This script processes CoreRuleSet rules from a specified directory and creates ConfigMaps
for each rule file that contains SecRule or SecAction directives. Individual rules
containing @pmFromFile directives can optionally be removed with warnings via
the --ignore-pmFromFile flag. Rules with specific IDs can also be excluded
via the --ignore-rules argument. The X-CRS-Test rule can be included via
the --include-test-rule flag.
"""

import argparse
import sys
import re
from pathlib import Path
from typing import List, Tuple, Set


# Base rules configmap content (from config/samples/ruleset.yaml)
# this Base rules also contains the extra SecAction for Coreruleset deployment
#
# NOTE: SecAuditLogRelevantStatus uses "^(40[0-3]|40[5-9]|4[1-9][0-9]|5[0-9][0-9])$" instead of
# the Perl-style "^(?:5|4(?!04))" because Coraza/Envoy WASM uses RE2 regex engine which does not
# support negative lookahead (?!). This pattern matches all 4xx and 5xx status codes except 404.
# The base rules present here are extracted from https://github.com/corazawaf/coraza-proxy-wasm/tree/3607c012f60c07263cd4d7a96e787e82b9047b7f/wasmplugin/rules
# - crs-setup.conf.example
# - coraza.conf-recommended.conf
BASE_RULES_CONFIGMAP = """apiVersion: v1
kind: ConfigMap
metadata:
  name: base-rules
data:
  rules: |
    SecRuleEngine On
    SecRequestBodyAccess On
    SecRequestBodyLimit 131072
    SecRequestBodyInMemoryLimit 131072
    SecRequestBodyLimitAction Reject
    SecResponseBodyAccess Off
    SecResponseBodyMimeType text/plain text/html text/xml
    SecResponseBodyLimit 524288
    SecResponseBodyLimitAction ProcessPartial
    SecAuditEngine RelevantOnly
    SecAuditLogType Serial
    SecAuditLog /dev/stdout
    SecAuditLogFormat JSON
    SecAuditLogParts ABIJDEFHZ
    SecAuditLogRelevantStatus "^(40[0-3]|40[5-9]|4[1-9][0-9]|5[0-9][0-9])$"
    SecRule REQUEST_HEADERS:Content-Type "^(?:application(?:/soap\\+|/)|text/)xml" \\
     "id:200000,\\
     phase:1,\\
     t:none,t:lowercase,\\
     pass,\\
     nolog,\\
     ctl:requestBodyProcessor=XML"
    SecRule REQUEST_HEADERS:Content-Type "^application/json" \\
     "id:200001,\\
     phase:1,\\
     t:none,t:lowercase,\\
     pass,\\
     nolog,\\
     ctl:requestBodyProcessor=JSON"
    SecRule REQUEST_HEADERS:Content-Type "^application/[a-z0-9.-]+[+]json" \\
     "id:200006,\\
     phase:1,\\
     t:none,t:lowercase,\\
     pass,\\
     nolog,\\
     ctl:requestBodyProcessor=JSON"
    SecRule REQBODY_ERROR "!@eq 0" \\
     "id:200002,\\
     phase:2,\\
     t:none,\\
     log,\\
     deny,\\
     status:400,\\
     msg:'Failed to parse request body.',\\
     logdata:'%{reqbody_error_msg}',\\
     severity:2"
    SecRule MULTIPART_STRICT_ERROR "!@eq 0" \\
     "id:200003,\\
     phase:2,\\
     t:none,\\
     log,\\
     deny,\\
     status:400,\\
     msg:'Multipart request body failed strict validation.'"
    SecDefaultAction "phase:2,log,auditlog,deny,status:403"
    SecAction \\
     "id:900120,\\
     phase:1,\\
     pass,\\
     t:none,\\
     nolog,\\
     tag:'OWASP_CRS',\\
     ver:'OWASP_CRS/4.23.0',\\
     setvar:tx.early_blocking=1"
    SecAction \\
     "id:900990,\\
     phase:1,\\
     pass,\\
     t:none,\\
     nolog,\\
     tag:'OWASP_CRS',\\
     ver:'OWASP_CRS/4.23.0',\\
     setvar:tx.crs_setup_version=4230"
"""

# X-CRS-Test rule (optional)
# Additional rules from https://github.com/corazawaf/coraza-proxy-wasm/blob/3607c012f60c07263cd4d7a96e787e82b9047b7f/wasmplugin/rules/ftw-config.conf
X_CRS_TEST_RULE = """    SecResponseBodyMimeType text/plain text/html text/xml application/json
    SecDefaultAction "phase:3,log,auditlog,pass"
    SecDefaultAction "phase:4,log,auditlog,pass"
    SecDefaultAction "phase:5,log,auditlog,pass"
    SecDebugLogLevel 3
    SecAction \\
     "id:900005,\\
     phase:1,\\
     nolog,\\
     pass,\\
     t:none,\\
     setvar:tx.blocking_paranoia_level=4,\\
     setvar:tx.crs_validate_utf8_encoding=1,\\
     setvar:tx.arg_name_length=100,\\
     setvar:tx.arg_length=400,\\
     setvar:tx.total_arg_length=64000,\\
     setvar:tx.max_num_args=255,\\
     setvar:tx.max_file_size=64100,\\
     setvar:tx.combined_file_sizes=65535,\\
     ctl:ruleEngine=DetectionOnly,\\
     ctl:ruleRemoveById=910000"
    SecRule REQUEST_HEADERS:X-CRS-Test "@rx ^.*$" \\
     "id:999999,\\
     pass,\\
     phase:1,\\
     log,\\
     msg:'X-CRS-Test %{MATCHED_VAR}',\\
     ctl:ruleRemoveById=1-999999"
"""


def get_rule_files(rules_dir: str) -> List[Path]:
    """Get all .conf files from the rules directory."""
    rules_path = Path(rules_dir)

    if not rules_path.exists():
        print(f"ERROR: Rules directory not found: {rules_path}", file=sys.stderr)
        sys.exit(1)

    if not rules_path.is_dir():
        print(f"ERROR: Path is not a directory: {rules_path}", file=sys.stderr)
        sys.exit(1)

    conf_files = sorted(rules_path.glob("*.conf"))
    print(f"Found {len(conf_files)} .conf files in {rules_path}", file=sys.stderr)
    return conf_files


def extract_rule_id(rule_text: str) -> str:
    """Extract the ID from a SecRule."""
    # Look for id:NUMBER pattern
    match = re.search(r'id:(\d+)', rule_text)
    if match:
        return match.group(1)
    return "unknown"


def split_into_rules(content: str) -> List[str]:
    """
    Split file content into individual rules/directives.

    Returns a list of text blocks, where each block is either:
    - A SecRule, SecAction, or other Sec* directive (potentially multi-line with backslash continuations)
    - Comments or blank lines
    """
    lines = content.split('\n')
    blocks = []
    current_block = []
    in_multiline = False

    for line in lines:
        stripped = line.rstrip()

        # Check if this is a continuation of a multi-line rule
        if in_multiline:
            current_block.append(line)
            if not stripped.endswith('\\'):
                in_multiline = False
                blocks.append('\n'.join(current_block))
                current_block = []
        # Check if this starts a Sec* directive (SecRule, SecAction, SecMarker, etc.)
        elif not stripped.startswith('#') and re.match(r'^(SecRule|SecAction|SecMarker)\b', stripped):
            current_block = [line]
            if stripped.endswith('\\'):
                in_multiline = True
            else:
                blocks.append('\n'.join(current_block))
                current_block = []
        # Other directives, comments, or blank lines
        else:
            blocks.append(line)

    # Add any remaining block
    if current_block:
        blocks.append('\n'.join(current_block))

    return blocks


def process_file_content(file_path: Path, ignore_rule_ids: Set[str], ignore_pmfromfile: bool) -> Tuple[str, List[Tuple[str, str]]]:
    """
    Process a rule file and remove rules with @pmFromFile or rules with IDs in the ignore list.

    Args:
        file_path: Path to the rule file
        ignore_rule_ids: Set of rule IDs to ignore
        ignore_pmfromfile: Whether to ignore rules containing @pmFromFile

    Returns:
        Tuple of (processed_content, list_of_(removed_rule_id, reason))
    """
    try:
        content = file_path.read_text(encoding='utf-8', errors='ignore')
    except Exception as e:
        print(f"ERROR: Failed to read {file_path}: {e}", file=sys.stderr)
        return "", []

    # Check if file has any SecRule or SecAction
    if "SecRule" not in content and "SecAction" not in content:
        return "", []

    blocks = split_into_rules(content)
    filtered_blocks = []
    removed_rules = []

    for block in blocks:
        stripped_block = block.strip()
        # Check if this block is a Sec* directive (SecRule, SecAction, etc.)
        if stripped_block and not stripped_block.startswith('#') and stripped_block.startswith('Sec'):
            # Check for @pmFromFile (only relevant for SecRule) if ignore flag is set
            if ignore_pmfromfile and stripped_block.startswith('SecRule') and '@pmFromFile' in block:
                rule_id = extract_rule_id(block)
                removed_rules.append((rule_id, "@pmFromFile not supported"))
            else:
                # Check if this Sec* directive has an ID in the ignore list
                rule_id = extract_rule_id(block)
                if rule_id in ignore_rule_ids:
                    removed_rules.append((rule_id, "Rule ID in ignore list"))
                else:
                    filtered_blocks.append(block)
        else:
            # Comments, blank lines, etc.
            filtered_blocks.append(block)

    processed_content = '\n'.join(filtered_blocks)
    return processed_content, removed_rules


def generate_configmap_name(file_path: Path) -> str:
    """
    Generate ConfigMap name from filename following Kubernetes DNS subdomain naming rules.

    According to RFC 1123, the name must:
    - Consist of lowercase alphanumeric characters, '-' or '.'
    - Start with an alphanumeric character
    - End with an alphanumeric character
    - Be at most 253 characters long
    """
    # Remove .conf extension and convert to lowercase
    name = file_path.stem.lower()

    # Replace underscores with hyphens (underscores are not allowed)
    name = name.replace('_', '-')

    # Remove any characters that are not lowercase alphanumeric, hyphen, or period
    name = re.sub(r'[^a-z0-9.-]', '', name)

    # Ensure it starts with an alphanumeric character
    name = re.sub(r'^[^a-z0-9]+', '', name)

    # Ensure it ends with an alphanumeric character
    name = re.sub(r'[^a-z0-9]+$', '', name)

    # Validate the resulting name
    if not name:
        raise ValueError(f"Cannot generate valid ConfigMap name from file: {file_path.name}")

    if len(name) > 253:
        raise ValueError(f"Generated ConfigMap name exceeds 253 characters: {name}")

    return name


def generate_configmap(file_path: Path, ignore_rule_ids: Set[str], ignore_pmfromfile: bool) -> Tuple[str, str, str]:
    """
    Generate a ConfigMap YAML for a rule file.

    Args:
        file_path: Path to the rule file
        ignore_rule_ids: Set of rule IDs to ignore
        ignore_pmfromfile: Whether to ignore rules containing @pmFromFile

    Returns:
        Tuple of (configmap_name, configmap_yaml, skip_reason)
        skip_reason will be empty string if not skipped
    """
    configmap_name = generate_configmap_name(file_path)

    processed_content, removed_rules = process_file_content(file_path, ignore_rule_ids, ignore_pmfromfile)

    # Log removed rules
    if removed_rules:
        print(f"  ⚠ WARNING: Ignored rules in {file_path.name}:", file=sys.stderr)
        for rule_id, reason in removed_rules:
            print(f"    - Rule ID: {rule_id} ({reason})", file=sys.stderr)

    if not processed_content.strip():
        return "", "", "No SecRule or SecAction directives found"

    # Indent the rules content for YAML
    indented_rules = "\n".join(f"    {line}" if line.strip() else "" for line in processed_content.splitlines())

    configmap = f"""apiVersion: v1
kind: ConfigMap
metadata:
  name: {configmap_name}
data:
  rules: |
{indented_rules}
"""

    return configmap_name, configmap, ""


def generate_ruleset(configmap_names: List[str], include_base_rules: bool = True) -> str:
    """Generate a RuleSet resource referencing all ConfigMaps."""
    rules_entries = []

    # Add base-rules as the first entry if requested
    if include_base_rules:
        rules_entries.append("    - name: base-rules")

    for name in configmap_names:
        rules_entries.append(f"    - name: {name}")

    rules_section = '\n'.join(rules_entries)

    ruleset = f"""apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSet
metadata:
  name: default-ruleset
spec:
  rules:
{rules_section}
"""

    return ruleset


def main():
    # Parse command-line arguments
    parser = argparse.ArgumentParser(
        description='Generate Kubernetes ConfigMaps from OWASP CoreRuleSet rules',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  # Generate ConfigMaps from a rules directory
  %(prog)s --rules-dir /path/to/coreruleset/rules/@owasp_crs

  # Ignore rules containing @pmFromFile directives
  %(prog)s --rules-dir /path/to/rules --ignore-pmFromFile

  # Ignore specific rule IDs
  %(prog)s --rules-dir /path/to/rules --ignore-rules 949110,949111,980130

  # Include X-CRS-Test rule and ignore @pmFromFile
  %(prog)s --rules-dir /path/to/rules --include-test-rule --ignore-pmFromFile
"""
    )
    parser.add_argument(
        '--ignore-rules',
        type=str,
        default='',
        help='Comma-separated list of rule IDs to ignore (e.g., "949110,949111,980130")'
    )
    parser.add_argument(
        '--ignore-pmFromFile',
        action='store_true',
        help='Ignore rules containing @pmFromFile directives (not supported by Coraza)'
    )
    parser.add_argument(
        '--include-test-rule',
        action='store_true',
        help='Include the X-CRS-Test rule in the base rules ConfigMap'
    )
    parser.add_argument(
        '--rules-dir',
        type=str,
        required=True,
        help='Directory containing the CoreRuleSet rules (e.g., /path/to/coreruleset/rules/@owasp_crs)'
    )
    args = parser.parse_args()

    # Parse ignore list
    ignore_rule_ids: Set[str] = set()
    if args.ignore_rules:
        ignore_rule_ids = {rid.strip() for rid in args.ignore_rules.split(',') if rid.strip()}
        if ignore_rule_ids:
            print(f"Ignoring rule IDs: {', '.join(sorted(ignore_rule_ids))}", file=sys.stderr)

    # Get all rule files
    rule_files = get_rule_files(args.rules_dir)

    # Process each file
    processed_count = 0
    skipped_count = 0
    configmaps = []
    configmap_names = []

    print(f"\nProcessing {len(rule_files)} files...\n", file=sys.stderr)

    for rule_file in rule_files:
        print(f"Processing: {rule_file.name}", file=sys.stderr)
        configmap_name, configmap_yaml, skip_reason = generate_configmap(rule_file, ignore_rule_ids, args.ignore_pmFromFile)

        if configmap_yaml:
            configmaps.append(configmap_yaml)
            configmap_names.append(configmap_name)
            processed_count += 1
            print(f"  ✓ Generated ConfigMap: {configmap_name}", file=sys.stderr)
        else:
            print(f"  ✗ Skipped: {skip_reason}", file=sys.stderr)
            skipped_count += 1

    # Generate RuleSet
    ruleset = generate_ruleset(configmap_names)

    # Output summary
    print(f"\n{'='*60}", file=sys.stderr)
    print(f"Summary:", file=sys.stderr)
    print(f"  Base rules: 1 (bundled)", file=sys.stderr)
    print(f"  Processed: {processed_count} files", file=sys.stderr)
    print(f"  Skipped: {skipped_count} files", file=sys.stderr)
    print(f"  Total ConfigMaps: {len(configmap_names) + 1}", file=sys.stderr)
    print(f"{'='*60}\n", file=sys.stderr)

    # Print base-rules ConfigMap first
    base_rules_output = BASE_RULES_CONFIGMAP.rstrip()
    if args.include_test_rule:
        base_rules_output += "\n" + X_CRS_TEST_RULE
    print(base_rules_output)

    # Print generated ConfigMaps
    for configmap in configmaps:
        print("---")
        print(configmap, end="")

    # Print RuleSet
    print("---")
    print(ruleset, end="")


if __name__ == "__main__":
    main()

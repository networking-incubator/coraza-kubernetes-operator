import yaml

# ---------------------------------------------------------------------------
# YAML: semver scalars for OLM CSV
# ---------------------------------------------------------------------------
#
# Unquoted "0.0.0" can round-trip through YAML→JSON as a number; CSV
# spec.version uses OperatorVersion, whose UnmarshalJSON only accepts a JSON
# string. Force double-quoted scalars so validation always sees a semver string.


class SemverYAML(str):
    """Semver string that PyYAML emits quoted (required for OLM CSV spec.version)."""


def _represent_semver_yaml(dumper, data):
    return dumper.represent_scalar("tag:yaml.org,2002:str", str(data), style='"')


yaml.add_representer(SemverYAML, _represent_semver_yaml)

# ---------------------------------------------------------------------------
# YAML Helpers
# ---------------------------------------------------------------------------


def load_yaml_docs(path: str) -> list:
    """Load a multi-document YAML file, filtering out empty documents."""
    with open(path) as f:
        return [d for d in yaml.safe_load_all(f) if d is not None]


def write_yaml_docs(path: str, docs: list):
    """Write multiple YAML documents to a file with explicit document markers."""
    with open(path, "w") as f:
        yaml.dump_all(
            docs, f, default_flow_style=False, sort_keys=False, explicit_start=True
        )


def write_yaml(path: str, data: dict):
    """Write a single YAML document to a file."""
    with open(path, "w") as f:
        yaml.dump(data, f, default_flow_style=False, sort_keys=False, width=1000)

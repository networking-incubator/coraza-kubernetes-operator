"""Tests for KIND cluster command-line validation."""

import argparse
import json
import os
import types
import unittest
from unittest import mock

import kind_cluster


class KindClusterNameTest(unittest.TestCase):
    def test_kind_cluster_name(self):
        self.assertEqual(kind_cluster.kind_cluster_name("coraza-integration-1"), "coraza-integration-1")
        with self.assertRaises(argparse.ArgumentTypeError):
            kind_cluster.kind_cluster_name("coraza;id")


class SailVersionTest(unittest.TestCase):
    def test_override_on_another_istio_minor_is_rejected(self):
        with mock.patch.dict(os.environ, {"SAIL_OPERATOR_VERSION": "1.30.1"}):
            with self.assertRaises(SystemExit):
                kind_cluster.resolve_sail_version("1.31.1")

    def test_invalid_override_format_is_rejected(self):
        with mock.patch.dict(os.environ, {"SAIL_OPERATOR_VERSION": "1.31"}):
            with self.assertRaises(SystemExit):
                kind_cluster.resolve_sail_version("1.31.1")

    def test_matching_override_is_used(self):
        with mock.patch.dict(os.environ, {"SAIL_OPERATOR_VERSION": "1.31.0"}):
            self.assertEqual(kind_cluster.resolve_sail_version("1.31.1"), "1.31.0")

    def test_default_picks_newest_chart_patch_of_the_istio_minor(self):
        charts = [
            {"name": "sail-operator/sail-operator", "version": "1.31.1"},
            {"name": "sail-operator/sail-operator", "version": "1.31.10"},
            {"name": "sail-operator/sail-operator", "version": "1.30.4"},
            {"name": "sail-operator/sail-operator", "version": "1.31.0-rc.1"},
        ]
        result = types.SimpleNamespace(returncode=0, stdout=json.dumps(charts))
        with mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop("SAIL_OPERATOR_VERSION", None)
            with mock.patch.object(kind_cluster, "run", return_value=result):
                self.assertEqual(kind_cluster.resolve_sail_version("1.31.1"), "1.31.10")

    def test_no_chart_for_the_istio_minor_fails_with_guidance(self):
        result = types.SimpleNamespace(returncode=0, stdout="[]")
        with mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop("SAIL_OPERATOR_VERSION", None)
            with mock.patch.object(kind_cluster, "run", return_value=result):
                with self.assertRaises(SystemExit):
                    kind_cluster.resolve_sail_version("1.31.1")


if __name__ == "__main__":
    unittest.main()

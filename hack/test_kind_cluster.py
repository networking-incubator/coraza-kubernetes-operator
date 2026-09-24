"""Tests for KIND cluster command-line validation."""

import argparse
import unittest

import kind_cluster


class KindClusterNameTest(unittest.TestCase):
    def test_kind_cluster_name(self):
        self.assertEqual(kind_cluster.kind_cluster_name("coraza-integration-1"), "coraza-integration-1")
        with self.assertRaises(argparse.ArgumentTypeError):
            kind_cluster.kind_cluster_name("coraza;id")


if __name__ == "__main__":
    unittest.main()

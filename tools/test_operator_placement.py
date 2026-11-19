"""Ownership and rollback controls for the disposable operator drain fixture."""

import unittest
from unittest import mock

import operator_placement as fixture


class PlacementCleanupTests(unittest.TestCase):
    def test_preexisting_name_refuses_before_creation_or_cleanup(self):
        seen = {"network": {"k3d-existing"}}
        with mock.patch.object(fixture, "cluster_resources", return_value=seen), \
                mock.patch.object(fixture, "remove_cluster") as cleanup:
            with self.assertRaisesRegex(RuntimeError, "pre-existing"):
                with fixture.fresh_cluster("existing", mock.Mock()):
                    self.fail("attempted creation under a pre-existing name")
            cleanup.assert_not_called()

    def test_failed_partial_create_removes_only_its_network_and_volume(self):
        cluster = "kz76-test"
        network = "k3d-" + cluster
        volume = network + "-images"
        inventory = {"container": set(), "network": set(), "volume": set()}
        calls = []

        def docker(args):
            calls.append(args)
            if args[:3] == ["docker", "network", "inspect"]:
                return "private-network-id\n" if args[-1] == "{{.Id}}" else '{"app":"k3d"}'
            if args[:3] == ["docker", "volume", "inspect"]:
                return '{"app":"k3d","k3d.cluster":"kz76-test"}'
            if args == ["docker", "network", "rm", "private-network-id"]:
                inventory["network"].remove(network)
            elif args == ["docker", "volume", "rm", volume]:
                inventory["volume"].remove(volume)
            else:
                self.fail(f"unexpected command: {args}")
            return ""

        with mock.patch.object(fixture, "cluster_resources", side_effect=lambda _: {
                k: set(v) for k, v in inventory.items()}), mock.patch.object(fixture, "run", side_effect=docker):
            k3d = mock.Mock()
            with self.assertRaisesRegex(RuntimeError, "create failed"):
                with fixture.fresh_cluster(cluster, k3d):
                    inventory["network"].add(network)
                    inventory["volume"].add(volume)
                    raise RuntimeError("create failed")
            k3d.assert_not_called()
        self.assertFalse(any(inventory.values()))
        self.assertIn(["docker", "volume", "rm", volume], calls)

    def test_foreign_node_label_refuses_destruction(self):
        with mock.patch.object(fixture, "cluster_resources", return_value={
                "container": {"k3d-x-server-0"}, "network": set(), "volume": set()}), \
                mock.patch.object(fixture, "run", return_value='{"app":"k3d","k3d.cluster":"other"}') as docker:
            k3d = mock.Mock()
            with self.assertRaisesRegex(RuntimeError, "foreign container"):
                fixture.remove_cluster("x", k3d)
            k3d.assert_not_called()
            self.assertEqual(docker.call_count, 1)


if __name__ == "__main__":
    unittest.main()

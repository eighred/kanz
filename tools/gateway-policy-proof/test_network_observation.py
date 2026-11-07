import errno
import unittest
from unittest.mock import Mock, patch

import network_observation as network


class ConnectionClassification(unittest.TestCase):
    def setUp(self):
        namespace = {}
        exec(compile(network.CONNECT_SOURCE, "<closed-connect-source>", "exec"), namespace)
        self.connect = namespace["connect_literal"]

    def test_exact_connect_categories(self):
        connection = Mock()
        with patch("socket.create_connection", return_value=connection) as create:
            self.assertEqual(self.connect("2001:db8::1", 8101, 1), (connection, "connected"))
            create.assert_called_once_with(("2001:db8::1", 8101), timeout=1)
        for error, expected in (
                (TimeoutError(), "timeout"),
                (ConnectionRefusedError(errno.ECONNREFUSED, "private"), "refused"),
                (OSError(errno.EHOSTUNREACH, "private"), "host_unreachable"),
                (OSError(errno.ENETUNREACH, "private"), "unknown"),
                (OSError(errno.EACCES, "private"), "unknown"),
                (OSError("timeout refused secret"), "unknown")):
            with self.subTest(expected=expected, errno=error.errno):
                with patch("socket.create_connection", side_effect=error):
                    self.assertEqual(self.connect("192.0.2.1", 8080, 2), (None, expected))

    def test_literal_and_closed_port_before_socket(self):
        for address, port, timeout in (("example.invalid", 8080, 1), ("fe80::1%eth0", 8080, 1),
                                       ("192.0.2.1", 443, 1), ("192.0.2.1", True, 1),
                                       ("192.0.2.1", 8080, 3)):
            with patch("socket.create_connection") as create:
                with self.assertRaises(ValueError):
                    self.connect(address, port, timeout)
                create.assert_not_called()

    def test_batch_shape_and_unknown_remain_distinct(self):
        self.assertEqual(network.batch(["unknown"] * 3), ["unknown"] * 3)
        self.assertNotIn("unknown", network.DENIALS)
        self.assertNotIn("connected", network.DENIALS)
        for value in (None, [], ["timeout"] * 4, ["error"] * 3, [1] * 3, {"timeout": 3}):
            with self.assertRaises(ValueError):
                network.batch(value)


if __name__ == "__main__":
    unittest.main()

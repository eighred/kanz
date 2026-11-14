import copy
import json
import tempfile
import unittest
from pathlib import Path

import projection

HERE = Path(__file__).resolve().parent


class ProjectionTest(unittest.TestCase):
    def setUp(self):
        self.mapping = json.loads((HERE / "fixture_mapping.json").read_text())
        self.events = []
        for row in self.mapping["datasets"]:
            self.events.append({"eventType": "COMPLETE", "inputs": None, "outputs": [{"namespace": row["namespace"], "name": row["name"], "facets": {"kanzSchema": {"_producer": "fixture", "_schemaURL": "fixture", "payloadSchemaRef": row["schema_ref"]}}}]})
        self.events[1]["inputs"] = [{"namespace": "kanz.customer", "name": "PersonProfile"}]

    def test_distinct_metadata_and_exact_refs(self):
        values = projection.project(self.mapping, self.events)
        owners = [p["aspect"]["json"]["owners"][0]["owner"] for p in values if p["aspectName"] == "ownership"]
        tags = [p["aspect"]["json"]["tags"][0]["tag"] for p in values if p["aspectName"] == "globalTags"]
        refs = [p["aspect"]["json"]["schemaName"] for p in values if p["aspectName"] == "schemaMetadata"]
        self.assertEqual(owners, ["urn:li:corpGroup:fixture-customer", "urn:li:corpGroup:fixture-risk"])
        self.assertEqual(tags, ["urn:li:tag:PII", "urn:li:tag:PUBLIC"])
        self.assertEqual(refs, ["customer.v1.PersonProfile:1", "risk.v1.CustomerExposure:1"])

    def test_actual_root_null_is_known_empty(self):
        values = projection.project(self.mapping, self.events)
        root = next(p for p in values if p["aspectName"] == "upstreamLineage")
        self.assertEqual(root["aspect"]["json"]["upstreams"], [])

    def test_missing_or_malformed_inputs_refuse(self):
        for value in ("bad", {}, 0):
            with self.subTest(value=value):
                self.events[0]["inputs"] = value
                with self.assertRaises(ValueError):
                    projection.project(self.mapping, self.events)
        del self.events[0]["inputs"]
        with self.assertRaises(ValueError):
            projection.project(self.mapping, self.events)

    def test_dataset_edges_are_a_union_across_events(self):
        additional = copy.deepcopy(self.events[1])
        additional["inputs"] = None
        self.events.append(additional)
        values = projection.project(self.mapping, self.events)
        result = [p for p in values if p["aspectName"] == "upstreamLineage"][1]
        self.assertEqual(result["aspect"]["json"]["upstreams"], [{"dataset": "urn:li:dataset:(urn:li:dataPlatform:kanz,kanz.customer/PersonProfile,DEV)", "type": "TRANSFORMED"}])

    def test_bounded_dataset_cycles_preserve_all_observed_edges(self):
        later = copy.deepcopy(self.events[0])
        later["inputs"] = [{"namespace": "kanz.risk", "name": "CustomerExposure"}]
        self.events.append(later)
        result = {p["entityUrn"]: p["aspect"]["json"]["upstreams"] for p in projection.project(self.mapping, self.events) if p["aspectName"] == "upstreamLineage"}
        profile = "urn:li:dataset:(urn:li:dataPlatform:kanz,kanz.customer/PersonProfile,DEV)"
        exposure = "urn:li:dataset:(urn:li:dataPlatform:kanz,kanz.risk/CustomerExposure,DEV)"
        self.assertEqual(result, {profile: [{"dataset": exposure, "type": "TRANSFORMED"}], exposure: [{"dataset": profile, "type": "TRANSFORMED"}]})

    def test_unknown_dataset_has_no_default(self):
        self.events[1]["outputs"][0]["name"] = "Unmapped"
        with self.assertRaises(ValueError):
            projection.project(self.mapping, self.events)

    def test_unknown_upstream_has_no_default(self):
        self.events[1]["inputs"][0]["name"] = "Unmapped"
        with self.assertRaises(ValueError):
            projection.project(self.mapping, self.events)

    def test_conflicting_schema_ref_refuses(self):
        self.events[0]["outputs"][0]["facets"]["kanzSchema"]["payloadSchemaRef"] = "customer.v1.PersonProfile:2"
        with self.assertRaises(ValueError):
            projection.project(self.mapping, self.events)

    def test_duplicate_mapping_refuses(self):
        self.mapping["datasets"].append(copy.deepcopy(self.mapping["datasets"][0]))
        with self.assertRaises(ValueError):
            projection.project(self.mapping, self.events)

    def test_separate_batches_preserve_tuple_identity(self):
        first = copy.deepcopy(self.mapping)
        first["datasets"] = [first["datasets"][0]]
        second = copy.deepcopy(first)
        second["datasets"][0].update(namespace="kanz", name="customer.PersonProfile")
        event = copy.deepcopy(self.events[0])
        event["outputs"][0].update(namespace="kanz", name="customer.PersonProfile")
        original = projection.project(first, [self.events[0]])[0]["entityUrn"]
        later = projection.project(second, [event])[0]["entityUrn"]
        self.assertEqual(original, "urn:li:dataset:(urn:li:dataPlatform:kanz,kanz.customer/PersonProfile,DEV)")
        self.assertEqual(later, "urn:li:dataset:(urn:li:dataPlatform:kanz,kanz/customer.PersonProfile,DEV)")
        self.assertNotEqual(original, later)

    def test_unknown_classification_and_fields_refuse(self):
        for field, value in (("sensitivity", "unknown"), ("steward", "*"), ("fields", [])):
            mapping = copy.deepcopy(self.mapping)
            mapping["datasets"][0][field] = value
            with self.subTest(field=field), self.assertRaises(ValueError):
                projection.project(mapping, self.events)

    def test_payload_and_arbitrary_facets_are_not_exported(self):
        self.events[0]["payload"] = "PRIVATE"
        with self.assertRaises(ValueError):
            projection.project(self.mapping, self.events)
        del self.events[0]["payload"]
        self.events[0]["outputs"][0]["facets"]["arbitrary"] = {"secret": "PRIVATE"}
        with self.assertRaises(ValueError):
            projection.project(self.mapping, self.events)

    def test_duplicate_keys_and_oversized_json_refuse(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "input.json"
            path.write_text('{"version":1,"version":2}')
            with self.assertRaises(ValueError):
                projection.load(path)
            path.write_bytes(b" " * (projection.LIMIT + 1))
            with self.assertRaises(ValueError):
                projection.load(path)

    def test_failed_batch_exposes_no_partial_file_or_overwrite(self):
        with tempfile.TemporaryDirectory() as directory:
            mapping, events, output = [Path(directory) / name for name in ("mapping.json", "events.json", "output.json")]
            mapping.write_text(json.dumps(self.mapping))
            self.events[1]["outputs"][0]["name"] = "Unknown"
            events.write_text(json.dumps(self.events))
            with self.assertRaises(ValueError):
                projection.write_projection(mapping, events, output)
            self.assertFalse(output.exists())
            output.write_text("preserve")
            self.events.pop()
            events.write_text(json.dumps(self.events))
            with self.assertRaises(FileExistsError):
                projection.write_projection(mapping, events, output)
            self.assertEqual(output.read_text(), "preserve")


if __name__ == "__main__":
    unittest.main()

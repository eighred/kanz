"""Bounded metadata-only OpenLineage projection into DataHub's MCP file source.

Mounted mappings are explicit deployment input, not discovery/policy authority.
Validate a complete batch before exposing any file to `datahub ingest`.
"""
import argparse
import hashlib
import json
import os
import re
from pathlib import Path

LIMIT = 1024 * 1024
IDENT = re.compile(r"[A-Za-z][A-Za-z0-9_.-]{0,127}\Z")
REF = re.compile(r"[A-Za-z][A-Za-z0-9_.]*:[1-9][0-9]{0,8}\Z")


def load(path):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate JSON key")
            result[key] = value
        return result
    with open(path, "rb") as stream:
        body = stream.read(LIMIT + 1)
    if len(body) > LIMIT:
        raise ValueError("metadata input exceeds limit")
    return json.loads(body, object_pairs_hook=unique)


def ident(value):
    if not isinstance(value, str) or not IDENT.fullmatch(value):
        raise ValueError("invalid metadata identifier")
    return value


def key(dataset):
    if not isinstance(dataset, dict) or set(dataset) - {"namespace", "name", "facets"}:
        raise ValueError("unexpected dataset fields")
    return ident(dataset["namespace"]), ident(dataset["name"])


def mappings(document):
    if not isinstance(document, dict) or set(document) != {"version", "environment", "datasets"}:
        raise ValueError("invalid mapping contract")
    if type(document["version"]) is not int or document["version"] != 1 or document["environment"] not in {"DEV", "TEST", "PROD"}:
        raise ValueError("unsupported mapping version/environment")
    rows = document["datasets"]
    if not isinstance(rows, list) or not 1 <= len(rows) <= 100:
        raise ValueError("invalid mapping count")
    result = {}
    for row in rows:
        if not isinstance(row, dict) or set(row) != {"namespace", "name", "schema_ref", "steward", "sensitivity", "fields"}:
            raise ValueError("invalid dataset mapping")
        dataset = key({"namespace": row["namespace"], "name": row["name"]})
        if dataset in result:
            raise ValueError("duplicate dataset mapping")
        ident(row["steward"])
        if not isinstance(row["schema_ref"], str) or not REF.fullmatch(row["schema_ref"]):
            raise ValueError("invalid schema reference")
        if row["sensitivity"] not in {"public", "pii"}:
            raise ValueError("unknown sensitivity")
        if not isinstance(row["fields"], list) or not 1 <= len(row["fields"]) <= 100:
            raise ValueError("invalid schema fields")
        names = set()
        for field in row["fields"]:
            if not isinstance(field, dict) or set(field) != {"name", "type"}:
                raise ValueError("invalid schema field")
            ident(field["name"])
            if field["name"] in names or field["type"] not in {"string", "boolean", "integer", "decimal"}:
                raise ValueError("duplicate/unsupported schema field")
            names.add(field["name"])
        result[dataset] = row
    return document["environment"], result


def urn(dataset, environment):
    # Both validated components exclude slash and URN delimiters. Unlike dot
    # flattening, this tuple encoding remains injective across separate batches.
    return f"urn:li:dataset:(urn:li:dataPlatform:kanz,{dataset[0]}/{dataset[1]},{environment})"


def project(document, events):
    environment, rows = mappings(document)
    if not isinstance(events, list) or not 1 <= len(events) <= 100:
        raise ValueError("invalid event count")
    observed, edges = {}, {}
    for event in events:
        if not isinstance(event, dict) or set(event) - {"eventType", "eventTime", "run", "job", "inputs", "outputs", "producer", "schemaURL"}:
            raise ValueError("unexpected event fields")
        # FromEnvelope marshals root inputs as null. That is a known empty
        # upstream set; other malformed shapes remain refusals.
        event_inputs = event.get("inputs")
        if event_inputs is None and "inputs" in event:
            event_inputs = []
        if event.get("eventType") != "COMPLETE" or not isinstance(event_inputs, list) or not isinstance(event.get("outputs"), list):
            raise ValueError("invalid completed event")
        if len(event_inputs) > 100 or len(event["outputs"]) != 1:
            raise ValueError("invalid lineage shape")
        inputs = set()
        for source in event_inputs:
            ds = key(source)
            if ds not in rows:
                raise ValueError("unmapped upstream dataset")
            inputs.add(ds)
        for output in event["outputs"]:
            ds = key(output)
            if ds not in rows:
                raise ValueError("unmapped output dataset")
            facets = output.get("facets")
            if not isinstance(facets, dict) or set(facets) != {"kanzSchema"}:
                raise ValueError("missing or unexpected schema facet")
            facet = facets["kanzSchema"]
            if not isinstance(facet, dict) or set(facet) != {"_producer", "_schemaURL", "payloadSchemaRef"}:
                raise ValueError("invalid schema facet")
            if facet["payloadSchemaRef"] != rows[ds]["schema_ref"]:
                raise ValueError("schema reference conflicts with explicit mapping")
            if ds in inputs:
                raise ValueError("self lineage")
            observed[ds] = rows[ds]
            edges.setdefault(ds, set()).update(inputs)
    if any(source not in observed for sources in edges.values() for source in sources):
        raise ValueError("upstream metadata absent from batch")
    proposals = []
    types = {"string": "StringType", "boolean": "BooleanType", "integer": "NumberType", "decimal": "NumberType"}
    for ds, row in sorted(observed.items()):
        raw_schema = json.dumps({"payloadSchemaRef": row["schema_ref"], "fields": row["fields"]}, sort_keys=True, separators=(",", ":"))
        fields = [{"fieldPath": field["name"], "nativeDataType": field["type"], "type": {"type": {"com.linkedin.schema." + types[field["type"]]: {}}}} for field in row["fields"]]
        aspects = {
            "datasetProperties": {"name": ds[1], "customProperties": {"kanz.namespace": ds[0], "payloadSchemaRef": row["schema_ref"]}},
            "ownership": {"owners": [{"owner": f"urn:li:corpGroup:{row['steward']}", "type": "DATAOWNER"}], "lastModified": {"time": 0, "actor": "urn:li:corpuser:kanz-catalog"}},
            "globalTags": {"tags": [{"tag": "urn:li:tag:" + ("PII" if row["sensitivity"] == "pii" else "PUBLIC")}]},
            "schemaMetadata": {"schemaName": row["schema_ref"], "platform": "urn:li:dataPlatform:kanz", "version": 0, "hash": hashlib.sha256(raw_schema.encode()).hexdigest(), "platformSchema": {"com.linkedin.schema.OtherSchema": {"rawSchema": raw_schema}}, "fields": fields},
            "upstreamLineage": {"upstreams": [{"dataset": urn(source, environment), "type": "TRANSFORMED"} for source in sorted(edges[ds])]},
        }
        for name, aspect in aspects.items():
            proposals.append({"entityType": "dataset", "entityUrn": urn(ds, environment), "changeType": "UPSERT", "aspectName": name, "aspect": {"json": aspect}})
    return proposals


def write_projection(mapping, events, destination):
    body = json.dumps(project(load(mapping), load(events)), sort_keys=True, separators=(",", ":")).encode()
    if len(body) > LIMIT:
        raise ValueError("projected metadata exceeds limit")
    descriptor = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "wb") as stream:
        stream.write(body)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--mapping", required=True, type=Path)
    parser.add_argument("--events", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    arguments = parser.parse_args()
    write_projection(arguments.mapping, arguments.events, arguments.output)

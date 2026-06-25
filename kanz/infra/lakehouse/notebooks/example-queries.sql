-- Example research queries over the lakehouse (LAKE-01d).
-- Run from the Jupyter notebook (%sql magic) or the Trino CLI against the
-- `hive` catalog. They read the lake-sink landing
-- ({domain}/{entity}/dt=YYYY-MM-DD/part-*.ndjson, LAKE-01a) as external tables.

-- 1. Register the schema + one external table over a landed entity. The columns
--    mirror the lake-sink Row: stable envelope columns + the nested `payload`
--    object (JSON), partitioned by the `dt` date. New payload fields appear
--    automatically because lake-sink decodes against the EVT-16 registry.
CREATE SCHEMA IF NOT EXISTS hive.kanz
WITH (location = 's3://kanz-lake/');

CREATE TABLE IF NOT EXISTS hive.kanz.market_marketdataevent (
  event_id           varchar,
  correlation_id     varchar,
  causation_id       varchar,
  domain             varchar,
  entity             varchar,
  event_type         varchar,
  event_class        varchar,
  tenant_id          varchar,
  source             varchar,
  partition_key      varchar,
  event_time         varchar,
  schema_version     integer,
  payload_schema_ref varchar,
  ingested_at        varchar,
  payload            varchar,   -- raw JSON; parse with json_extract
  decode_error       varchar,
  dt                 varchar
)
WITH (
  external_location = 's3://kanz-lake/market/MarketDataEvent/',
  format = 'JSON',
  partitioned_by = ARRAY['dt']
);

-- Hive external tables need their partitions discovered after new dt= dirs land.
CALL hive.system.sync_partition_metadata('kanz', 'market_marketdataevent', 'ADD');

-- 2. Daily event volume by tenant — a quick data-completeness sanity check.
SELECT dt, tenant_id, count(*) AS events
FROM hive.kanz.market_marketdataevent
WHERE dt BETWEEN '2026-06-01' AND '2026-06-30'
GROUP BY dt, tenant_id
ORDER BY dt, tenant_id;

-- 3. Pull a payload field out of the decoded JSON (schema-evolution-friendly:
--    json_extract tolerates fields that only appear after a schema bump).
SELECT
  event_id,
  partition_key AS instrument_id,
  event_time,
  json_extract_scalar(payload, '$.price.coefficient') AS price_coefficient,
  json_extract_scalar(payload, '$.price.exponent')    AS price_exponent
FROM hive.kanz.market_marketdataevent
WHERE dt = '2026-06-25'
  AND decode_error IS NULL
ORDER BY event_time
LIMIT 100;

-- 4. Surface any rows that failed payload decode (poison messages lake-sink
--    landed envelope-only) so a data steward can triage the schema/registry gap.
SELECT dt, payload_schema_ref, count(*) AS undecoded
FROM hive.kanz.market_marketdataevent
WHERE decode_error IS NOT NULL
GROUP BY dt, payload_schema_ref
ORDER BY undecoded DESC;

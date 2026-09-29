-- #1296: stream newest visible identities before LIMIT, without sorting lifetime history.
-- Knowledge order must remain DESC while time/kind are DESC; reversing the
-- existing ascending-history index would select oldest revisions first.
CREATE INDEX IF NOT EXISTS price_observations_recent_idx
 ON price_observations (instrument_id, observation_time DESC, kind DESC, knowledge_time DESC);

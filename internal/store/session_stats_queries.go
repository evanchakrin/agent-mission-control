package store

// Empty identities count here, matching the historical detail view (Calendar
// deliberately excludes them). Summary rows retain even zero-token identities.
const sessionAgentCountSQL = `SELECT COUNT(*) FROM (
 SELECT a.agent_id FROM query_event_agents a JOIN query_sessions q ON q.id=a.session_id AND q.source_id=a.source_id AND q.generation=a.generation AND q.projection_revision=a.projection_revision WHERE q.id=?
 UNION SELECT u.agent_id FROM query_agent_usage u JOIN query_sessions q ON q.id=u.session_id AND q.source_id=u.source_id AND q.generation=u.generation AND q.projection_revision=u.projection_revision WHERE q.id=?)`

// Separate MIN/MAX permit index endpoint lookups. Exclude only empty timestamps,
// as the original MIN/MAX(NULLIF(timestamp,”)) did, not Calendar's zero sentinel.
const sessionUsageBoundsSQL = `SELECT
 (SELECT MIN(u.timestamp) FROM query_usage u WHERE u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision AND u.timestamp>''),
 (SELECT MAX(u.timestamp) FROM query_usage u WHERE u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision AND u.timestamp>'')
 FROM query_sessions q WHERE q.id=?`

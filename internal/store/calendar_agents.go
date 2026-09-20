package store

// Seek the next agent in the existing covering index instead of visiting every
// token observation in a long session. Each recursive step strictly advances;
// there is no agent-count cap. Include event-only agents and exclude empty IDs.
const calendarAgentScopeCount = `(WITH RECURSIVE usage_agents(agent_id) AS (
 SELECT MIN(u.agent_id) FROM query_usage u WHERE u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision AND u.agent_id>''
 UNION ALL
 SELECT (SELECT MIN(u.agent_id) FROM query_usage u WHERE u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision AND u.agent_id>usage_agents.agent_id)
 FROM usage_agents WHERE agent_id IS NOT NULL)
 SELECT COUNT(*) FROM (
 SELECT a.agent_id FROM query_event_agents a WHERE a.session_id=q.id AND a.source_id=q.source_id AND a.generation=q.generation AND a.projection_revision=q.projection_revision AND a.agent_id<>''
 UNION SELECT agent_id FROM usage_agents WHERE agent_id IS NOT NULL))`

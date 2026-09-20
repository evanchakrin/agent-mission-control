package store

// Caller supplies a selected CTE with one current catalog row per session.
// Classify only directly evidenced prices from each selected immutable snapshot.
const selectedTierCTEs = `
 snapshots AS MATERIALIZED (SELECT q.id AS session_id,a.id AS snapshot_id,json_extract(a.estimate,'$.catalogId') AS catalog_id
 FROM selected q JOIN sessions s ON s.id=q.id JOIN accounting_estimates a ON a.id=json_extract(s.projection,'$.pricing.snapshotId') AND a.session_id=q.id
 WHERE q.pricing_known=1 AND json_extract(a.estimate,'$.attributionVersion')=1),
 rates AS MATERIALIZED (SELECT c.id AS catalog_id,json_extract(r.value,'$.id') AS rate_id,json_extract(r.value,'$.tier') AS tier
 FROM rate_catalogs c JOIN (SELECT DISTINCT catalog_id FROM snapshots) used ON used.catalog_id=c.id JOIN json_each(c.catalog,'$.rates') r),
 tiers AS (SELECT q.id AS session_id,
 SUM(CASE WHEN r.tier IN ('flagship','premium') THEN json_extract(p.evidence,'$.cost') ELSE 0 END) AS top_cost,
 SUM(json_extract(p.evidence,'$.cost')) AS known_cost,
 SUM(json_extract(p.evidence,'$.pricedTokens')) AS tier_tokens
 FROM selected q JOIN snapshots a ON a.session_id=q.id
 JOIN query_usage u ON u.session_id=q.id AND u.source_id=q.source_id AND u.generation=q.generation AND u.projection_revision=q.projection_revision
 JOIN observation_prices p ON p.snapshot_id=a.snapshot_id AND p.observation_id=u.id AND p.agent_id=u.agent_id AND p.model=u.model
 AND json_extract(p.evidence,'$.recordedTokens')=u.tokens_in+u.tokens_cache+u.tokens_write+u.tokens_out
 JOIN rates r ON r.catalog_id=a.catalog_id AND r.rate_id=json_extract(p.evidence,'$.rateIds[0]')
 WHERE json_array_length(p.evidence,'$.rateIds')=1 AND r.tier IN ('flagship','premium','mid','cheap') AND json_extract(p.evidence,'$.pricedTokens')>0
 GROUP BY q.id),
`

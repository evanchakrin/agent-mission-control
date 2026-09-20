package store

// Session JSON is selected only after the catalog page is bounded. Older
// projections omitted forkedFromId even though their durable parser checkpoint
// already held it. Read that field without rewriting history or reindexing raw
// bytes. The sources and active_projection lookups use their composite PKs.
// Never borrow identity from a different generation or unpublished checkpoint.
const sessionProjectionJSON = `CASE
 WHEN s.provider='codex' AND COALESCE(json_extract(s.projection,'$.forkedFromId'),'')=''
 THEN json_set(s.projection,'$.forkedFromId',COALESCE((
  SELECT CASE WHEN json_valid(src.parser_state) THEN
   CASE WHEN json_extract(src.parser_state,'$.nativeId')=json_extract(s.projection,'$.nativeId')
     AND json_type(src.parser_state,'$.forkedFromId')='text'
     AND length(json_extract(src.parser_state,'$.forkedFromId')) BETWEEN 1 AND 512
    THEN json_extract(src.parser_state,'$.forkedFromId') ELSE '' END
   ELSE '' END
  FROM sources src WHERE src.source_id=s.source_id AND src.generation=s.generation
   AND COALESCE((SELECT ap.revision FROM active_projection ap WHERE ap.source_id=src.source_id AND ap.generation=src.generation),'')=COALESCE(json_extract(s.projection,'$.projectionRevision'),'')
 ),''))
 ELSE s.projection END`

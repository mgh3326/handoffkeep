package store

// syncMigrations installs row triggers so all writers (including cascade deletes
// and single-statement upserts) journal in the mutation's own transaction.
// The singleton row lock serializes ID allocation through commit: a sequence
// alone allows a later ID to commit first and be skipped by a delta reader.
var syncMigrations = []string{
	`CREATE TABLE sync_log (sync_id BIGSERIAL PRIMARY KEY, model TEXT NOT NULL CHECK(model IN ('document','task','task_comment','decision')), model_id TEXT NOT NULL, action TEXT NOT NULL CHECK(action IN ('I','U','D')), at TIMESTAMPTZ NOT NULL)`,
	`CREATE INDEX sync_log_at ON sync_log(at,sync_id)`,
	`CREATE TABLE sync_state (singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK(singleton), max_sync_id BIGINT NOT NULL DEFAULT 0, pruned_through BIGINT NOT NULL DEFAULT 0)`,
	`INSERT INTO sync_state(singleton) VALUES(true)`,
	`CREATE TABLE sync_decisions (id TEXT PRIMARY KEY, task_id BIGINT NOT NULL UNIQUE, updated_at TIMESTAMPTZ NOT NULL)`,
	`INSERT INTO sync_decisions SELECT refs->'decision_request'->>'id',id,updated_at FROM tasks WHERE COALESCE(refs->'decision_request'->>'id','')<>''`,
	`CREATE FUNCTION handoffkeep_sync_emit() RETURNS trigger LANGUAGE plpgsql AS $$
 DECLARE row_id TEXT; next_id BIGINT;
 BEGIN
   PERFORM 1 FROM sync_state WHERE singleton FOR UPDATE;
   IF TG_OP='DELETE' THEN row_id := OLD.id::text; ELSE row_id := NEW.id::text; END IF;
   INSERT INTO sync_log(model,model_id,action,at) VALUES(TG_ARGV[0],row_id,substr(TG_OP,1,1),clock_timestamp()) RETURNING sync_id INTO next_id;
   UPDATE sync_state SET max_sync_id=next_id WHERE singleton;
   RETURN NULL;
 END $$`,
	`CREATE TRIGGER sync_documents AFTER INSERT OR UPDATE OR DELETE ON documents FOR EACH ROW EXECUTE FUNCTION handoffkeep_sync_emit('document')`,
	`CREATE TRIGGER sync_tasks AFTER INSERT OR UPDATE OR DELETE ON tasks FOR EACH ROW EXECUTE FUNCTION handoffkeep_sync_emit('task')`,
	`CREATE TRIGGER sync_comments AFTER INSERT OR UPDATE OR DELETE ON task_comments FOR EACH ROW EXECUTE FUNCTION handoffkeep_sync_emit('task_comment')`,
	`CREATE TRIGGER sync_decisions AFTER INSERT OR UPDATE OR DELETE ON sync_decisions FOR EACH ROW EXECUTE FUNCTION handoffkeep_sync_emit('decision')`,
	`CREATE FUNCTION handoffkeep_sync_decision() RETURNS trigger LANGUAGE plpgsql AS $$
 DECLARE request_id TEXT;
 BEGIN
   IF TG_OP='DELETE' THEN DELETE FROM sync_decisions WHERE task_id=OLD.id; RETURN NULL; END IF;
   request_id := NEW.refs->'decision_request'->>'id';
   IF TG_OP='UPDATE' AND OLD.refs->'decision_request' IS NOT DISTINCT FROM NEW.refs->'decision_request' THEN RETURN NULL; END IF;
   DELETE FROM sync_decisions WHERE task_id=NEW.id AND id IS DISTINCT FROM request_id;
   IF COALESCE(request_id,'')<>'' THEN
     INSERT INTO sync_decisions(id,task_id,updated_at) VALUES(request_id,NEW.id,NEW.updated_at)
     ON CONFLICT(id) DO UPDATE SET updated_at=EXCLUDED.updated_at;
   END IF;
   RETURN NULL;
 END $$`,
	`CREATE TRIGGER sync_task_decision AFTER INSERT OR UPDATE OR DELETE ON tasks FOR EACH ROW EXECUTE FUNCTION handoffkeep_sync_decision()`,
	`INSERT INTO schema_version(version) VALUES (29)`,
}

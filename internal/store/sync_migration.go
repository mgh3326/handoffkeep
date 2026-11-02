package store

// syncMigrations installs row triggers so all writers (including cascade deletes
// and single-statement upserts) journal in the mutation's own transaction.
// A deferred trigger allocates ordered feed IDs at commit. Source mutations
// only append pending rows; the shared advisory lock is acquired by the final
// allocator, after all ordinary transaction work has finished.
var syncMigrations = []string{
	`CREATE SEQUENCE sync_log_sync_id_seq`,
	`CREATE TABLE sync_log (entry_id BIGSERIAL PRIMARY KEY, txid XID8 NOT NULL DEFAULT pg_current_xact_id(), sync_id BIGINT UNIQUE, model TEXT NOT NULL CHECK(model IN ('document','task','task_comment','decision')), model_id TEXT NOT NULL, action TEXT NOT NULL CHECK(action IN ('I','U','D')), at TIMESTAMPTZ NOT NULL)`,
	`CREATE INDEX sync_log_at ON sync_log(at,sync_id) WHERE sync_id IS NOT NULL`,
	`CREATE INDEX sync_log_pending ON sync_log(txid,entry_id) WHERE sync_id IS NULL`,
	`CREATE TABLE sync_state (singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK(singleton), max_sync_id BIGINT NOT NULL DEFAULT 0, pruned_through BIGINT NOT NULL DEFAULT 0)`,
	`INSERT INTO sync_state(singleton) VALUES(true)`,
	`CREATE TABLE sync_decisions (id TEXT PRIMARY KEY, task_id BIGINT NOT NULL UNIQUE, updated_at TIMESTAMPTZ NOT NULL)`,
	`INSERT INTO sync_decisions SELECT DISTINCT ON (refs->'decision_request'->>'id') refs->'decision_request'->>'id',id,updated_at FROM tasks WHERE COALESCE(refs->'decision_request'->>'id','')<>'' ORDER BY refs->'decision_request'->>'id',updated_at DESC,id DESC`,
	`CREATE FUNCTION handoffkeep_sync_emit() RETURNS trigger LANGUAGE plpgsql AS $$
 DECLARE row_id TEXT;
 BEGIN
   IF TG_OP='DELETE' THEN row_id := OLD.id::text; ELSE row_id := NEW.id::text; END IF;
   INSERT INTO sync_log(model,model_id,action,at) VALUES(TG_ARGV[0],row_id,substr(TG_OP,1,1),clock_timestamp());
   RETURN NULL;
 END $$`,
	`CREATE FUNCTION handoffkeep_sync_allocate() RETURNS trigger LANGUAGE plpgsql AS $$
 DECLARE pending RECORD; last_id BIGINT;
 BEGIN
   IF NOT EXISTS (SELECT 1 FROM sync_log WHERE txid=pg_current_xact_id() AND sync_id IS NULL) THEN RETURN NULL; END IF;
   PERFORM pg_advisory_xact_lock(824180067);
   FOR pending IN SELECT entry_id FROM sync_log WHERE txid=pg_current_xact_id() AND sync_id IS NULL ORDER BY entry_id LOOP
     UPDATE sync_log SET sync_id=nextval('sync_log_sync_id_seq') WHERE entry_id=pending.entry_id RETURNING sync_id INTO last_id;
   END LOOP;
   UPDATE sync_state SET max_sync_id=last_id WHERE singleton;
   RETURN NULL;
 END $$`,
	`CREATE CONSTRAINT TRIGGER sync_allocate AFTER INSERT ON sync_log DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION handoffkeep_sync_allocate()`,
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
	`INSERT INTO schema_version(version) VALUES (30)`,
}

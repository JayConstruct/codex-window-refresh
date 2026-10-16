PRAGMA application_id=0x43575232;
CREATE TABLE IF NOT EXISTS schema_version(version INTEGER NOT NULL);
INSERT INTO schema_version SELECT 2 WHERE NOT EXISTS(SELECT 1 FROM schema_version);
CREATE TABLE IF NOT EXISTS accounts(
 id TEXT PRIMARY KEY, auth_index TEXT NOT NULL, status TEXT NOT NULL,
 quota TEXT, observed_at TEXT, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS jobs(
 id TEXT PRIMARY KEY, idempotency_key TEXT UNIQUE NOT NULL, request_hash TEXT NOT NULL,
 target TEXT NOT NULL, phase TEXT NOT NULL, expanded INTEGER NOT NULL DEFAULT 0,
 error_code TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_pending ON jobs(expanded,created_at);
CREATE TABLE IF NOT EXISTS query_runs(
 id TEXT PRIMARY KEY, account_id TEXT NOT NULL, auth_index TEXT NOT NULL, phase TEXT NOT NULL, started_at TEXT NOT NULL,
 outcome_hash TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS one_query_per_account ON query_runs(account_id) WHERE phase='running';
CREATE TABLE IF NOT EXISTS job_items(
 job_id TEXT NOT NULL REFERENCES jobs(id), account_id TEXT NOT NULL,
 phase TEXT NOT NULL, run_id TEXT REFERENCES query_runs(id), error_code TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(job_id,account_id)
);
CREATE INDEX IF NOT EXISTS items_queue ON job_items(phase,account_id);
CREATE INDEX IF NOT EXISTS items_run ON job_items(run_id);
CREATE INDEX IF NOT EXISTS jobs_retention ON jobs(phase,updated_at);
CREATE TABLE IF NOT EXISTS view_state(
 singleton INTEGER PRIMARY KEY CHECK(singleton=1), revision INTEGER NOT NULL,
 accounts_revision INTEGER NOT NULL
);
INSERT OR IGNORE INTO view_state VALUES(1,0,0);
CREATE TABLE IF NOT EXISTS event_state(
 singleton INTEGER PRIMARY KEY CHECK(singleton=1), bridge_instance TEXT NOT NULL DEFAULT '',
 host_process TEXT NOT NULL DEFAULT '', sequence INTEGER NOT NULL DEFAULT 0,
 loss_epoch INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'observing',
 reason TEXT NOT NULL DEFAULT 'initial_observation', connected INTEGER NOT NULL DEFAULT 0,
 connection_error TEXT NOT NULL DEFAULT '', last_seen TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO event_state(singleton) VALUES(1);
CREATE TABLE IF NOT EXISTS retired_bridges(instance TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS activity_requests(
 bridge_instance TEXT NOT NULL, request_id TEXT NOT NULL, identity TEXT NOT NULL,
 account_id TEXT NOT NULL, phase TEXT NOT NULL CHECK(phase IN ('active','unknown','completed')),
 started_at TEXT NOT NULL, last_event_at TEXT NOT NULL,
 PRIMARY KEY(bridge_instance,request_id)
);
CREATE INDEX IF NOT EXISTS activity_phase ON activity_requests(phase,last_event_at);
CREATE INDEX IF NOT EXISTS activity_identity ON activity_requests(identity,phase);
CREATE INDEX IF NOT EXISTS activity_account ON activity_requests(account_id,phase);
CREATE TABLE IF NOT EXISTS activity_users(identity TEXT PRIMARY KEY,last_activity TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS activity_recent ON activity_users(last_activity);
CREATE TABLE IF NOT EXISTS job_receipts(
 idempotency_key TEXT PRIMARY KEY, request_hash TEXT NOT NULL, job_id TEXT NOT NULL,
 expires_at INTEGER NOT NULL
);
INSERT OR IGNORE INTO job_receipts SELECT idempotency_key,request_hash,id,unixepoch(created_at)+2592000 FROM jobs;
CREATE INDEX IF NOT EXISTS receipts_expiry ON job_receipts(expires_at);
CREATE TRIGGER IF NOT EXISTS accounts_insert AFTER INSERT ON accounts BEGIN
 UPDATE view_state SET revision=revision+1,accounts_revision=accounts_revision+1;
END;
CREATE TRIGGER IF NOT EXISTS accounts_delete AFTER DELETE ON accounts BEGIN
 UPDATE view_state SET revision=revision+1,accounts_revision=accounts_revision+1;
END;
CREATE TRIGGER IF NOT EXISTS accounts_update AFTER UPDATE ON accounts
 WHEN OLD.auth_index IS NOT NEW.auth_index OR OLD.status IS NOT NEW.status
 OR OLD.quota IS NOT NEW.quota OR OLD.observed_at IS NOT NEW.observed_at BEGIN
 UPDATE view_state SET revision=revision+1,accounts_revision=accounts_revision+1;
END;
CREATE TRIGGER IF NOT EXISTS jobs_insert AFTER INSERT ON jobs BEGIN UPDATE view_state SET revision=revision+1; END;
CREATE TRIGGER IF NOT EXISTS jobs_update AFTER UPDATE ON jobs BEGIN UPDATE view_state SET revision=revision+1; END;
CREATE TRIGGER IF NOT EXISTS jobs_delete AFTER DELETE ON jobs BEGIN UPDATE view_state SET revision=revision+1; END;
CREATE TRIGGER IF NOT EXISTS items_insert AFTER INSERT ON job_items BEGIN UPDATE view_state SET revision=revision+1; END;
CREATE TRIGGER IF NOT EXISTS items_update AFTER UPDATE ON job_items BEGIN UPDATE view_state SET revision=revision+1; END;
CREATE TRIGGER IF NOT EXISTS activity_insert AFTER INSERT ON activity_requests BEGIN UPDATE view_state SET revision=revision+1; END;
CREATE TRIGGER IF NOT EXISTS activity_update AFTER UPDATE ON activity_requests BEGIN UPDATE view_state SET revision=revision+1; END;
CREATE TRIGGER IF NOT EXISTS activity_delete AFTER DELETE ON activity_requests BEGIN UPDATE view_state SET revision=revision+1; END;
CREATE TRIGGER IF NOT EXISTS event_update AFTER UPDATE ON event_state
 WHEN OLD.bridge_instance IS NOT NEW.bridge_instance OR OLD.sequence IS NOT NEW.sequence
 OR OLD.loss_epoch IS NOT NEW.loss_epoch OR OLD.status IS NOT NEW.status OR OLD.reason IS NOT NEW.reason
 OR OLD.connected IS NOT NEW.connected OR OLD.connection_error IS NOT NEW.connection_error BEGIN
 UPDATE view_state SET revision=revision+1;
END;

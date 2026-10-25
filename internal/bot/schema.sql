CREATE TABLE IF NOT EXISTS users (
 id INTEGER PRIMARY KEY, started_at INTEGER NOT NULL, start_day TEXT NOT NULL,
 enabled INTEGER NOT NULL DEFAULT 1, pending_claim INTEGER
);
CREATE TABLE IF NOT EXISTS promos (
 code TEXT PRIMARY KEY, day TEXT NOT NULL, user_id INTEGER REFERENCES users(id), issued_at INTEGER,
 UNIQUE(day,user_id)
);
CREATE INDEX IF NOT EXISTS promos_available ON promos(day,user_id);
CREATE TABLE IF NOT EXISTS promo_requests (
 id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id), day TEXT NOT NULL, created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS entries (
 day TEXT NOT NULL, partner TEXT NOT NULL, user_id INTEGER NOT NULL REFERENCES users(id), registered_at INTEGER NOT NULL,
 PRIMARY KEY(day,user_id), UNIQUE(partner,user_id)
);
CREATE TABLE IF NOT EXISTS entry_confirmations (
 day TEXT NOT NULL, user_id INTEGER NOT NULL, confirmed_at INTEGER NOT NULL,
 PRIMARY KEY(day,user_id), FOREIGN KEY(day,user_id) REFERENCES entries(day,user_id)
);
CREATE TABLE IF NOT EXISTS activity (
 day TEXT NOT NULL, user_id INTEGER NOT NULL REFERENCES users(id), partner TEXT NOT NULL, created_at INTEGER NOT NULL,
 PRIMARY KEY(day,user_id)
);
CREATE TABLE IF NOT EXISTS draws (
 id TEXT PRIMARY KEY, created_at INTEGER NOT NULL, approved_at INTEGER, approved_by TEXT
);
CREATE TABLE IF NOT EXISTS draw_entries (
 draw_id TEXT NOT NULL REFERENCES draws(id), user_id INTEGER NOT NULL REFERENCES users(id), weight INTEGER NOT NULL CHECK(weight BETWEEN 1 AND 10),
 PRIMARY KEY(draw_id,user_id)
);
CREATE TABLE IF NOT EXISTS winners (
 id INTEGER PRIMARY KEY, draw_id TEXT NOT NULL REFERENCES draws(id), user_id INTEGER NOT NULL REFERENCES users(id),
 rank INTEGER NOT NULL, prize TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN ('pending','reserve','notified','claimed','expired')),
 deadline INTEGER, full_name TEXT, consent_at INTEGER, consent_text TEXT,
 UNIQUE(draw_id,user_id), UNIQUE(draw_id,rank)
);
CREATE TABLE IF NOT EXISTS inbox (
 id TEXT PRIMARY KEY, user_id INTEGER NOT NULL, payload BLOB NOT NULL, created_at INTEGER NOT NULL, processed_at INTEGER,
 attempts INTEGER NOT NULL DEFAULT 0, next_attempt INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS inbox_pending_user ON inbox(user_id,processed_at);
CREATE INDEX IF NOT EXISTS inbox_pending_time ON inbox(processed_at,created_at);
CREATE TABLE IF NOT EXISTS outbox (
 id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id), payload BLOB NOT NULL,
 winner_id INTEGER UNIQUE REFERENCES winners(id), created_at INTEGER NOT NULL, sent_at INTEGER, failed_at INTEGER,
 broadcast INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0, next_attempt INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS outbox_pending ON outbox(sent_at,next_attempt);
CREATE INDEX IF NOT EXISTS outbox_user_order ON outbox(user_id,sent_at,failed_at);
CREATE TABLE IF NOT EXISTS broadcasts (id TEXT PRIMARY KEY, text TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS audit (
 id INTEGER PRIMARY KEY, actor TEXT NOT NULL, action TEXT NOT NULL, target TEXT NOT NULL, created_at INTEGER NOT NULL
);

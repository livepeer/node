-- UP
CREATE TABLE signer_kafka_state (
 id INTEGER PRIMARY KEY CHECK(id=1), signer TEXT NOT NULL, broker TEXT NOT NULL, topic TEXT NOT NULL,
 pending_count INTEGER NOT NULL DEFAULT 0 CHECK(pending_count>=0),
 pending_bytes INTEGER NOT NULL DEFAULT 0 CHECK(pending_bytes>=0), probe INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE signer_kafka_events (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL UNIQUE,
 payload BLOB NOT NULL, created_ms INTEGER NOT NULL
);
CREATE TRIGGER signer_kafka_insert AFTER INSERT ON signer_kafka_events BEGIN
 UPDATE signer_kafka_state SET pending_count=pending_count+1,pending_bytes=pending_bytes+length(NEW.payload) WHERE id=1;
END;
CREATE TRIGGER signer_kafka_delete AFTER DELETE ON signer_kafka_events BEGIN
 UPDATE signer_kafka_state SET pending_count=pending_count-1,pending_bytes=pending_bytes-length(OLD.payload) WHERE id=1;
END;
-- DOWN
DROP TRIGGER signer_kafka_delete;
DROP TRIGGER signer_kafka_insert;
DROP TABLE signer_kafka_events;
DROP TABLE signer_kafka_state;

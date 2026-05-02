//! # Local Cache — Offline Resilience
use rusqlite::{Connection, Result as SqlResult};

pub struct LocalCache { conn: Connection }

impl LocalCache {
    pub fn new(db_path: &str) -> SqlResult<Self> {
        let conn = Connection::open(db_path)?;
        let cache = Self { conn };
        cache.initialize_schema()?;
        Ok(cache)
    }

    pub fn in_memory() -> SqlResult<Self> {
        let conn = Connection::open_in_memory()?;
        let cache = Self { conn };
        cache.initialize_schema()?;
        Ok(cache)
    }

    fn initialize_schema(&self) -> SqlResult<()> {
        self.conn.execute_batch(
            "CREATE TABLE IF NOT EXISTS telemetry_buffer (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                node_id TEXT NOT NULL,
                reading_type TEXT NOT NULL,
                payload_json TEXT NOT NULL,
                timestamp INTEGER NOT NULL,
                synced INTEGER DEFAULT 0
            );
            CREATE TABLE IF NOT EXISTS local_state (
                node_id TEXT PRIMARY KEY,
                node_class TEXT NOT NULL,
                last_known_state TEXT NOT NULL,
                updated_at INTEGER NOT NULL
            );"
        )?;
        Ok(())
    }

    pub fn buffer_telemetry(&self, node_id: &str, reading_type: &str, payload_json: &str, timestamp: u64) -> SqlResult<()> {
        self.conn.execute(
            "INSERT INTO telemetry_buffer (node_id, reading_type, payload_json, timestamp) VALUES (?1, ?2, ?3, ?4)",
            (node_id, reading_type, payload_json, timestamp as i64),
        )?;
        Ok(())
    }

    pub fn unsynced_count(&self) -> SqlResult<u64> {
        let count: i64 = self.conn.query_row("SELECT COUNT(*) FROM telemetry_buffer WHERE synced = 0", [], |row| row.get(0))?;
        Ok(count as u64)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cache_buffers_and_counts() {
        let cache = LocalCache::in_memory().unwrap();
        assert_eq!(cache.unsynced_count().unwrap(), 0);
        cache.buffer_telemetry("node-123", "solar", r#"{"power_watts": 5000}"#, 1700000000).unwrap();
        assert_eq!(cache.unsynced_count().unwrap(), 1);
    }
}

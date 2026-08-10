//! # Local Cache — Offline Resilience
use rusqlite::{Connection, OptionalExtension, Result as SqlResult};

use crate::environment::Environment;

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
                synced INTEGER DEFAULT 0,
                environment TEXT NOT NULL DEFAULT 'production'
            );
            CREATE TABLE IF NOT EXISTS local_state (
                node_id TEXT PRIMARY KEY,
                node_class TEXT NOT NULL,
                last_known_state TEXT NOT NULL,
                updated_at INTEGER NOT NULL
            );
            -- Environment and tracking, as last pushed down by the control
            -- plane. Single-row (id = 1). It is persisted rather than held in
            -- memory so a machine muted on Friday does not come back tracked
            -- on Monday after a restart and page whoever is on call.
            CREATE TABLE IF NOT EXISTS agent_environment (
                id INTEGER PRIMARY KEY CHECK (id = 1),
                name TEXT NOT NULL,
                monitored INTEGER NOT NULL,
                note TEXT NOT NULL DEFAULT '',
                updated_at INTEGER NOT NULL
            );"
        )?;
        Ok(())
    }

    /// Persist the environment/tracking state pushed down by the control
    /// plane. Replaces whatever was there — there is only ever one answer.
    pub fn set_environment(&self, env: &Environment, now: u64) -> SqlResult<()> {
        self.conn.execute(
            "INSERT INTO agent_environment (id, name, monitored, note, updated_at)
             VALUES (1, ?1, ?2, ?3, ?4)
             ON CONFLICT(id) DO UPDATE SET
                name = excluded.name,
                monitored = excluded.monitored,
                note = excluded.note,
                updated_at = excluded.updated_at",
            (&env.name, i64::from(env.monitored), &env.note, now as i64),
        )?;
        Ok(())
    }

    /// Read back the stored environment. An agent that has never been told
    /// anything is production and tracked — the same assumption every other
    /// part of the stack makes about an unclassified machine.
    pub fn environment(&self) -> SqlResult<Environment> {
        let row = self
            .conn
            .query_row(
                "SELECT name, monitored, note FROM agent_environment WHERE id = 1",
                [],
                |row| {
                    Ok(Environment {
                        name: row.get(0)?,
                        monitored: row.get::<_, i64>(1)? != 0,
                        note: row.get(2)?,
                    })
                },
            )
            .optional()?;
        Ok(row.unwrap_or_default())
    }

    /// Buffer one telemetry reading, stamped with the environment it came
    /// from. Returns whether it was actually stored.
    ///
    /// While tracking is off, nothing is stored and `false` comes back. That
    /// is the point: the operator asked for this machine to stop counting,
    /// and the cheapest honest way to deliver that is to not produce the
    /// readings in the first place — not to produce them and hope the far end
    /// remembers to filter them out.
    pub fn buffer_telemetry(&self, node_id: &str, reading_type: &str, payload_json: &str, timestamp: u64) -> SqlResult<bool> {
        let env = self.environment()?;
        if !env.reports_telemetry() {
            return Ok(false);
        }
        self.conn.execute(
            "INSERT INTO telemetry_buffer (node_id, reading_type, payload_json, timestamp, environment) VALUES (?1, ?2, ?3, ?4, ?5)",
            (node_id, reading_type, payload_json, timestamp as i64, &env.name),
        )?;
        Ok(true)
    }

    pub fn unsynced_count(&self) -> SqlResult<u64> {
        let count: i64 = self.conn.query_row("SELECT COUNT(*) FROM telemetry_buffer WHERE synced = 0", [], |row| row.get(0))?;
        Ok(count as u64)
    }

    /// Unsynced readings for one environment only, so a lab tier's buffer can
    /// be inspected — or dropped — without touching production's.
    pub fn unsynced_count_for(&self, environment: &str) -> SqlResult<u64> {
        let count: i64 = self.conn.query_row(
            "SELECT COUNT(*) FROM telemetry_buffer WHERE synced = 0 AND environment = ?1",
            [environment],
            |row| row.get(0),
        )?;
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
        assert!(cache.buffer_telemetry("node-123", "solar", r#"{"power_watts": 5000}"#, 1700000000).unwrap());
        assert_eq!(cache.unsynced_count().unwrap(), 1);
    }

    #[test]
    fn a_fresh_agent_is_production_and_tracked() {
        let cache = LocalCache::in_memory().unwrap();
        let env = cache.environment().unwrap();
        assert_eq!(env.name, "production");
        assert!(env.monitored);
    }

    #[test]
    fn untracked_agent_buffers_nothing() {
        let cache = LocalCache::in_memory().unwrap();
        cache
            .set_environment(
                &Environment::from_params([("monitored", "false"), ("note", "lab rebuild")]),
                1_700_000_000,
            )
            .unwrap();

        let stored = cache
            .buffer_telemetry("node-123", "solar", r#"{"power_watts": 5000}"#, 1_700_000_001)
            .unwrap();
        assert!(!stored, "an untracked agent must not produce readings");
        assert_eq!(cache.unsynced_count().unwrap(), 0);
    }

    #[test]
    fn tracking_resumes_without_losing_the_earlier_buffer() {
        let cache = LocalCache::in_memory().unwrap();
        cache.buffer_telemetry("node-1", "solar", "{}", 1).unwrap();

        cache
            .set_environment(&Environment::from_params([("monitored", "false")]), 2)
            .unwrap();
        cache.buffer_telemetry("node-1", "solar", "{}", 3).unwrap();
        assert_eq!(cache.unsynced_count().unwrap(), 1, "the mute drops readings, not history");

        cache
            .set_environment(&Environment::from_params([("monitored", "true")]), 4)
            .unwrap();
        assert!(cache.buffer_telemetry("node-1", "solar", "{}", 5).unwrap());
        assert_eq!(cache.unsynced_count().unwrap(), 2);
    }

    #[test]
    fn readings_are_stamped_with_their_environment() {
        let cache = LocalCache::in_memory().unwrap();
        cache.buffer_telemetry("node-1", "solar", "{}", 1).unwrap();

        cache
            .set_environment(&Environment::from_params([("environment", "development")]), 2)
            .unwrap();
        cache.buffer_telemetry("node-1", "solar", "{}", 3).unwrap();

        // The whole point of the stamp: two readings from the same agent that
        // must never end up inside the same average.
        assert_eq!(cache.unsynced_count_for("production").unwrap(), 1);
        assert_eq!(cache.unsynced_count_for("development").unwrap(), 1);
    }

    #[test]
    fn environment_survives_a_reopen() {
        let dir = std::env::temp_dir().join("linexus-agent-env-test.sqlite");
        let _ = std::fs::remove_file(&dir);
        let path = dir.to_string_lossy().to_string();

        {
            let cache = LocalCache::new(&path).unwrap();
            cache
                .set_environment(
                    &Environment::from_params([
                        ("environment", "staging"),
                        ("monitored", "false"),
                        ("note", "migration weekend"),
                    ]),
                    1_700_000_000,
                )
                .unwrap();
        }

        let cache = LocalCache::new(&path).unwrap();
        let env = cache.environment().unwrap();
        assert_eq!(env.name, "staging");
        assert!(!env.monitored, "a machine muted on Friday stays muted on Monday");
        assert_eq!(env.note, "migration weekend");

        let _ = std::fs::remove_file(&path);
    }
}

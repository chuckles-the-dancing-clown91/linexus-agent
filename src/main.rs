//! # Linexus Agent — The Edge Data Plane

mod providers;
mod cache;
mod environment;

use cache::LocalCache;
use environment::Environment;

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt().with_target(true).with_level(true).init();
    tracing::info!("=== LINEXUS AGENT — Edge Data Plane ===");

    // The agent reads its own environment out of the local cache before it
    // does anything else. This is the state the control plane pushed down —
    // which tier this machine belongs to, and whether it is being tracked at
    // all — and it survives restarts on purpose: a machine muted on Friday
    // must not come back tracked on Monday and page whoever is on call.
    let cache = match LocalCache::in_memory() {
        Ok(c) => c,
        Err(e) => {
            tracing::error!(error = %e, "local cache unavailable");
            return;
        }
    };
    let env = cache.environment().unwrap_or_default();
    tracing::info!(environment = %env, "local SQLite cache: ACTIVE");
    if !env.reports_telemetry() {
        tracing::warn!(
            "telemetry suspended — this machine is not being tracked; \
             readings are dropped rather than buffered"
        );
    }

    use linexus_core::identity::{InfraProduct, GenerationType, NodeClass};
    let solar = NodeClass::InfraGenerator {
        product: InfraProduct::SolarArray { peak_kw: 32, efficiency_pct: 0.95 },
        generation_type: GenerationType::Solar,
        location_id: uuid::Uuid::new_v4(),
    };
    tracing::info!("Registered node class: {:?}", solar);
}

/// Apply an `agent.environment` plan step — the action the orchestrator emits
/// for a `set_environment` intent dispatched from Daedalus IT.
///
/// Returns the state that was applied, so the caller can report it back up the
/// chain. The Hub only marks a machine's environment as synced when it hears
/// this outcome; nothing else may claim it, because nothing else knows.
pub fn apply_environment_step<'a, I>(
    cache: &LocalCache,
    params: I,
    now: u64,
) -> Result<Environment, rusqlite::Error>
where
    I: IntoIterator<Item = (&'a str, &'a str)>,
{
    let env = Environment::from_params(params);
    cache.set_environment(&env, now)?;
    tracing::info!(environment = %env, "environment applied");
    Ok(env)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn applying_a_step_persists_and_takes_effect_immediately() {
        let cache = LocalCache::in_memory().unwrap();
        assert!(cache.buffer_telemetry("n1", "solar", "{}", 1).unwrap());

        let env = apply_environment_step(
            &cache,
            [
                ("environment", "development"),
                ("monitored", "false"),
                ("note", "lab rebuild"),
            ],
            2,
        )
        .unwrap();

        assert_eq!(env.name, "development");
        assert!(!env.monitored);
        assert_eq!(cache.environment().unwrap(), env);
        assert!(
            !cache.buffer_telemetry("n1", "solar", "{}", 3).unwrap(),
            "the step has to take effect on the very next reading, not at the next restart"
        );
    }

    #[test]
    fn a_step_can_bring_a_machine_back() {
        let cache = LocalCache::in_memory().unwrap();
        apply_environment_step(&cache, [("monitored", "false"), ("note", "down")], 1).unwrap();
        let env = apply_environment_step(&cache, [("environment", "production")], 2).unwrap();

        assert!(env.monitored, "an instruction with no tracking field leaves the machine loud");
        assert!(env.note.is_empty(), "the stale reason does not follow it back");
        assert!(cache.buffer_telemetry("n1", "solar", "{}", 3).unwrap());
    }
}

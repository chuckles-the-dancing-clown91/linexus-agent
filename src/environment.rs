//! # Environment & tracking — what this machine is, and whether it counts
//!
//! Every other module here answers a question the agent can work out on its
//! own: how much power the array is making, how full the buffer is. This one
//! is different. It holds two things the agent is *told*:
//!
//!   * `environment` — production, staging, development, or whatever tier
//!     this deployment calls it. It is stamped on outgoing telemetry so the
//!     far end can keep a lab reading out of a production average, rather
//!     than blending the two and calling the result a fleet.
//!   * `monitored` — whether the machine counts at all. A box that was
//!     powered down on purpose is not an outage, and while tracking is off
//!     this agent stops buffering telemetry entirely.
//!
//! That second point is the whole reason this state lives on the agent and
//! not only in a database somewhere upstream. An agent that does not know it
//! is untracked keeps shipping readings, and every one of them is a number
//! that has to be filtered out again on the far side by someone who remembers
//! to. Silence is cheaper and more honest than a filter.
//!
//! The state is persisted in the local SQLite cache, so it survives a restart
//! — a machine muted on Friday does not wake up on Monday and start paging
//! whoever is on call.

use std::fmt;

/// The default for a machine nobody has classified: production, and tracked.
/// The safe reading of an unlabelled box is that it matters.
pub const DEFAULT_ENVIRONMENT: &str = "production";

/// The environment/tracking state of this agent.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Environment {
    pub name: String,
    pub monitored: bool,
    /// Why tracking is off. Empty while tracked.
    pub note: String,
}

impl Default for Environment {
    fn default() -> Self {
        Self {
            name: DEFAULT_ENVIRONMENT.to_string(),
            monitored: true,
            note: String::new(),
        }
    }
}

impl fmt::Display for Environment {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        if self.monitored {
            write!(f, "{} (tracked)", self.name)
        } else if self.note.is_empty() {
            write!(f, "{} (not tracked)", self.name)
        } else {
            write!(f, "{} (not tracked — {})", self.name, self.note)
        }
    }
}

impl Environment {
    /// Build the state from a plan step's params, as the orchestrator emits
    /// them for the `agent.environment` action.
    ///
    /// Both fields degrade toward the loud option. An empty or missing
    /// environment becomes production, and anything that is not an explicit
    /// negative leaves the machine tracked — a malformed parameter must never
    /// be the reason a production box goes quiet.
    pub fn from_params<'a, I>(params: I) -> Self
    where
        I: IntoIterator<Item = (&'a str, &'a str)>,
    {
        let mut env = Self::default();
        let mut note = String::new();
        for (k, v) in params {
            match k {
                "environment" => {
                    let v = v.trim();
                    if !v.is_empty() {
                        env.name = v.to_lowercase();
                    }
                }
                "monitored" => {
                    env.monitored = !matches!(
                        v.trim().to_ascii_lowercase().as_str(),
                        "false" | "0" | "no" | "off"
                    );
                }
                "note" => note = v.trim().to_string(),
                _ => {}
            }
        }
        // A note only means anything while tracking is off. Carrying a stale
        // "lab rebuild" on a live machine is worse than carrying nothing.
        env.note = if env.monitored { String::new() } else { note };
        env
    }

    /// Whether this agent should be emitting telemetry at all.
    pub const fn reports_telemetry(&self) -> bool {
        self.monitored
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn defaults_to_tracked_production() {
        let env = Environment::default();
        assert_eq!(env.name, "production");
        assert!(env.monitored);
        assert!(env.reports_telemetry());
    }

    #[test]
    fn parses_a_full_untracked_instruction() {
        let env = Environment::from_params([
            ("environment", "Development"),
            ("monitored", "false"),
            ("note", "  lab rebuild  "),
        ]);
        assert_eq!(env.name, "development"); // normalized
        assert!(!env.monitored);
        assert_eq!(env.note, "lab rebuild");
        assert!(!env.reports_telemetry());
    }

    #[test]
    fn missing_params_leave_the_machine_loud() {
        let env = Environment::from_params([]);
        assert_eq!(env.name, "production");
        assert!(env.monitored);
    }

    #[test]
    fn garbage_tracking_value_leaves_the_machine_tracked() {
        // The failure mode this guards against is a typo silently muting a
        // production machine, which nobody would notice until an outage.
        let env = Environment::from_params([("monitored", "maybe")]);
        assert!(env.monitored);
    }

    #[test]
    fn turning_tracking_back_on_drops_the_stale_note() {
        let env = Environment::from_params([
            ("monitored", "true"),
            ("note", "unplugged for the reception rebuild"),
        ]);
        assert!(env.monitored);
        assert!(env.note.is_empty());
    }

    #[test]
    fn empty_environment_falls_back_rather_than_blanking() {
        let env = Environment::from_params([("environment", "   ")]);
        assert_eq!(env.name, "production");
    }
}

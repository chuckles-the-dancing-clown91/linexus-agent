//! Solar Inverter Provider
use serde::{Deserialize, Serialize};
use uuid::Uuid;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SolarReading {
    pub node_id: Uuid, pub timestamp: u64, pub power_watts: f64,
    pub energy_kwh_today: f64, pub efficiency_pct: f32, pub panel_temp_c: f32,
}

pub struct SolarProvider { pub node_id: Uuid }
impl SolarProvider {
    pub fn new(node_id: Uuid) -> Self { Self { node_id } }
    pub fn read_telemetry(&self) -> SolarReading {
        SolarReading { node_id: self.node_id, timestamp: 0, power_watts: 0.0, energy_kwh_today: 0.0, efficiency_pct: 0.0, panel_temp_c: 0.0 }
    }
}

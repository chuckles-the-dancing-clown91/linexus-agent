//! Biodigester Provider
use serde::{Deserialize, Serialize};
use uuid::Uuid;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BiodigesterReading {
    pub node_id: Uuid, pub timestamp: u64, pub biogas_m3_today: f64,
    pub internal_temp_c: f32, pub ph_level: f32,
}

pub struct BiodigesterProvider { pub node_id: Uuid }
impl BiodigesterProvider {
    pub fn new(node_id: Uuid) -> Self { Self { node_id } }
    pub fn read_telemetry(&self) -> BiodigesterReading {
        BiodigesterReading { node_id: self.node_id, timestamp: 0, biogas_m3_today: 0.0, internal_temp_c: 0.0, ph_level: 0.0 }
    }
}

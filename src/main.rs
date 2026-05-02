//! # Linexus Agent — The Edge Data Plane

mod providers;
mod cache;

use tracing_subscriber;

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt().with_target(true).with_level(true).init();
    tracing::info!("=== LINEXUS AGENT — Edge Data Plane ===");
    tracing::info!("Local SQLite cache: ACTIVE");

    use linexus_core::identity::{InfraProduct, GenerationType, NodeClass};
    let solar = NodeClass::InfraGenerator {
        product: InfraProduct::SolarArray { peak_kw: 32, efficiency_pct: 0.95 },
        generation_type: GenerationType::Solar,
        location_id: uuid::Uuid::new_v4(),
    };
    tracing::info!("Registered node class: {:?}", solar);
}

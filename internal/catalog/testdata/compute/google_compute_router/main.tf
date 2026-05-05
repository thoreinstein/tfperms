# Fixture for catalog entry:
#   - resources/google_compute_router

resource "google_compute_router" "main" {
  name    = "tfperms-test-router"
  region  = "us-central1"
  network = "default"

  bgp {
    asn = 64514
  }
}

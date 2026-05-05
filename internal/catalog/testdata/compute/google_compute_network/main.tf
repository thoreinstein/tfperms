# Fixture for catalog entry:
#   - resources/google_compute_network

resource "google_compute_network" "vpc" {
  name                    = "tfperms-test-vpc"
  auto_create_subnetworks = false
}

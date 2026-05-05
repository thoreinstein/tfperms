# Fixture for catalog entry:
#   - resources/google_compute_subnetwork

resource "google_compute_subnetwork" "primary" {
  name          = "tfperms-test-subnet"
  ip_cidr_range = "10.0.0.0/16"
  region        = "us-central1"
  network       = "tfperms-test-vpc"
}

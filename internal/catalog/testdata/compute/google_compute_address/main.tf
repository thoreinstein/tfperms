# Fixture for catalog entry:
#   - resources/google_compute_address

resource "google_compute_address" "vip" {
  name   = "tfperms-test-address"
  region = "us-central1"
}

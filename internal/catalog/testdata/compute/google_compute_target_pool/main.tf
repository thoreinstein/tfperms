# Fixture for catalog entry:
#   - resources/google_compute_target_pool

resource "google_compute_target_pool" "default" {
  name   = "tfperms-test-target-pool"
  region = "us-central1"
}

# Fixture for catalog entry:
#   - resources/google_compute_disk

resource "google_compute_disk" "data" {
  name = "tfperms-test-disk"
  type = "pd-balanced"
  zone = "us-central1-a"
  size = 10
}

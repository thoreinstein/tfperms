# Fixture for catalog entry:
#   - resources/google_compute_instance_group

resource "google_compute_instance_group" "static" {
  name = "tfperms-test-instance-group"
  zone = "us-central1-a"
}

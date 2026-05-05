# Fixture for catalog entry:
#   - resources/google_compute_instance_template

resource "google_compute_instance_template" "default" {
  name         = "tfperms-test-template"
  machine_type = "e2-small"

  disk {
    source_image = "debian-cloud/debian-12"
    auto_delete  = true
    boot         = true
  }

  network_interface {
    network = "default"
  }
}

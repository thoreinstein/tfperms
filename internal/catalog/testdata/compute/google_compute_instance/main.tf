# Fixture for catalog entry:
#   - resources/google_compute_instance

resource "google_compute_instance" "vm" {
  name         = "tfperms-test-vm"
  machine_type = "e2-medium"
  zone         = "us-central1-a"

  boot_disk {
    initialize_params {
      image = "debian-cloud/debian-12"
    }
  }

  network_interface {
    network = "default"
  }
}

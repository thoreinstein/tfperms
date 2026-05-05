# Fixture for catalog entry:
#   - resources/google_compute_firewall

resource "google_compute_firewall" "allow_ssh" {
  name    = "tfperms-test-allow-ssh"
  network = "default"

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }

  source_ranges = ["35.235.240.0/20"]
}

# Fixture for catalog entry:
#   - resources/google_compute_forwarding_rule

resource "google_compute_forwarding_rule" "internal" {
  name        = "tfperms-test-forwarding-rule"
  region      = "us-central1"
  ip_protocol = "TCP"
  ports       = ["80"]
}

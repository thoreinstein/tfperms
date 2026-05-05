# Fixture for catalog entry:
#   - resources/google_compute_global_address

resource "google_compute_global_address" "lb_ip" {
  name = "tfperms-test-global-address"
}

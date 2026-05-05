# Fixture for catalog entry:
#   - resources/google_compute_route

resource "google_compute_route" "egress" {
  name             = "tfperms-test-route"
  network          = "default"
  dest_range       = "10.99.0.0/16"
  next_hop_gateway = "default-internet-gateway"
  priority         = 1000
}

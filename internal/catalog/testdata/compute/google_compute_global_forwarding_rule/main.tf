# Fixture for catalog entry:
#   - resources/google_compute_global_forwarding_rule

resource "google_compute_global_forwarding_rule" "https" {
  name        = "tfperms-test-global-forwarding-rule"
  port_range  = "443"
  ip_protocol = "TCP"
  target      = "projects/example/global/targetHttpsProxies/tfperms-test-target"
}

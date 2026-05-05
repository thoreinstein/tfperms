# Fixture for catalog entry:
#   - resources/google_compute_health_check

resource "google_compute_health_check" "tcp" {
  name = "tfperms-test-health-check"

  tcp_health_check {
    port = 8080
  }
}

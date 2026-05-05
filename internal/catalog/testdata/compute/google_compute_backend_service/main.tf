# Fixture for catalog entry:
#   - resources/google_compute_backend_service

resource "google_compute_backend_service" "default" {
  name                  = "tfperms-test-backend-service"
  protocol              = "HTTP"
  timeout_sec           = 10
  load_balancing_scheme = "EXTERNAL_MANAGED"
}

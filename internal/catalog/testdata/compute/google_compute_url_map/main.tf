# Fixture for catalog entry:
#   - resources/google_compute_url_map

resource "google_compute_url_map" "default" {
  name            = "tfperms-test-url-map"
  default_service = "projects/example/global/backendServices/tfperms-test-backend-service"
}

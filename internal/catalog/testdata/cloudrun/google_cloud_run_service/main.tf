# Fixture for catalog entry:
#   - resources/google_cloud_run_service

resource "google_cloud_run_service" "api" {
  name     = "tfperms-test-api"
  location = "us-central1"

  template {
    spec {
      containers {
        image = "gcr.io/cloudrun/hello"
      }
    }
  }
}

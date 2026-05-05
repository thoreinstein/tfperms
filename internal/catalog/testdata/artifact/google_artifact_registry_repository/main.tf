# Fixture for catalog entry:
#   - resources/google_artifact_registry_repository

resource "google_artifact_registry_repository" "docker" {
  location      = "us-central1"
  repository_id = "tfperms-test-docker"
  format        = "DOCKER"
}

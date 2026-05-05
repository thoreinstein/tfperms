# Fixture for catalog entry:
#   - resources/google_secret_manager_secret_version

resource "google_secret_manager_secret_version" "api_key_v1" {
  secret      = "projects/example/secrets/tfperms-test-secret"
  secret_data = "tfperms-test-payload"
}

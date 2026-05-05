# Fixture for catalog entry:
#   - resources/google_secret_manager_secret

resource "google_secret_manager_secret" "api_key" {
  secret_id = "tfperms-test-secret"

  replication {
    auto {}
  }
}

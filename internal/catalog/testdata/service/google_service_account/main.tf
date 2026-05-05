# Fixture for catalog entry:
#   - resources/google_service_account

resource "google_service_account" "runtime" {
  account_id   = "tfperms-test-sa"
  display_name = "tfperms test service account"
}

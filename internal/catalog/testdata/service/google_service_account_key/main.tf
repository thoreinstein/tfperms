# Fixture for catalog entry:
#   - resources/google_service_account_key

resource "google_service_account_key" "runtime" {
  service_account_id = "projects/example/serviceAccounts/tfperms-test-sa@example.iam.gserviceaccount.com"
  key_algorithm      = "KEY_ALG_RSA_2048"
}

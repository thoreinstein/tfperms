# Fixture for catalog entry:
#   - resources/google_bigquery_dataset

resource "google_bigquery_dataset" "primary" {
  dataset_id = "tfperms_test_dataset"
  location   = "US"
}

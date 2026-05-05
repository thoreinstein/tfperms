# Fixture for catalog entry:
#   - resources/google_bigquery_table

resource "google_bigquery_table" "events" {
  dataset_id = "tfperms_test_dataset"
  table_id   = "events"
}

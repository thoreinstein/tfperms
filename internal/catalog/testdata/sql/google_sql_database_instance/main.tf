# Fixture for catalog entry:
#   - resources/google_sql_database_instance

resource "google_sql_database_instance" "primary" {
  name             = "tfperms-test-sql"
  database_version = "POSTGRES_15"
  region           = "us-central1"

  settings {
    tier = "db-custom-2-7680"
  }
}

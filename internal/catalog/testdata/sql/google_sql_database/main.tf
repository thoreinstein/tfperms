# Fixture for catalog entry:
#   - resources/google_sql_database

resource "google_sql_database" "app" {
  name     = "tfperms-test-db"
  instance = "tfperms-test-instance"
}

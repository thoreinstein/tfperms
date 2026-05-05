# Fixture for catalog entry:
#   - resources/google_sql_user

resource "google_sql_user" "app" {
  name     = "tfperms-test-user"
  instance = "tfperms-test-instance"
  password = "tfperms-test-password"
}

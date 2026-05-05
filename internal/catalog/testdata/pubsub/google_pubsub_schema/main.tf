# Fixture for catalog entry:
#   - resources/google_pubsub_schema

resource "google_pubsub_schema" "events" {
  name       = "tfperms-test-schema"
  type       = "AVRO"
  definition = jsonencode({
    type   = "record"
    name   = "Event"
    fields = [{ name = "id", type = "string" }]
  })
}

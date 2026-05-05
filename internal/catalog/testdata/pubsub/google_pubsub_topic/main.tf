# Fixture for catalog entry:
#   - resources/google_pubsub_topic

resource "google_pubsub_topic" "events" {
  name = "tfperms-test-events"
}

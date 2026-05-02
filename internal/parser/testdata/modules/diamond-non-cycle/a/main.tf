module "common" {
  source = "../shared"
}

resource "google_pubsub_topic" "in_a" {
  name = "diamond-non-cycle-in-a"
}

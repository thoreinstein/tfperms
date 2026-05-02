module "b" {
  source = "./b"
}

resource "google_pubsub_topic" "in_a" {
  name = "topic-from-a"
}

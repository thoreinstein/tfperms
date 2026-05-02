resource "google_storage_bucket" "data" {
  name     = "data-bucket"
  location = "US"
}

resource "google_storage_bucket_iam_binding" "data_viewers" {
  bucket = google_storage_bucket.data.name
  role   = "roles/storage.objectViewer"
  members = [
    "user:alice@example.com",
    "user:bob@example.com",
  ]
}

resource "google_storage_bucket_iam_member" "single" {
  bucket = google_storage_bucket.data.name
  role   = "roles/storage.admin"
  member = "user:carol@example.com"
}

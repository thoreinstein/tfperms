resource "google_storage_bucket" "kept" {
  name = "kept"
}

resource "google_storage_bucket" "dropped_map" {
  for_each = {}
  name     = each.key
}

resource "google_storage_bucket" "dropped_list" {
  for_each = []
  name     = each.value
}

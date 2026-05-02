resource "google_storage_bucket" "b" {
  name = "with-dynamics"

  dynamic "lifecycle_rule" {
    for_each = []
    content {
      action {
        type = "Delete"
      }
    }
  }

  dynamic "cors" {
    for_each = []
    content {
      origin = ["*"]
    }
  }

  dynamic "labels" {
    for_each = {}
    content {
      key   = labels.key
      value = labels.value
    }
  }
}

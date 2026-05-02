resource "google_storage_bucket" "live" {
  name = "live-bucket"
}

# resource "google_storage_bucket" "single_line_comment" {
#   name = "ignored"
# }

/*
resource "google_storage_bucket" "block_comment" {
  name = "also-ignored"
}
*/

// resource "google_storage_bucket" "double_slash" {
//   name = "ignored-too"
// }

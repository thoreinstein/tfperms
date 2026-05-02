module "from_git" {
  source = "git::https://github.com/org/repo.git"
}

resource "google_storage_bucket" "root" {
  name = "git-root"
}

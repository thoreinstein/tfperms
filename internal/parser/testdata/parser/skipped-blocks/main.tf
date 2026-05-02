terraform {
  required_version = ">= 1.0"
}

provider "google" {
  project = "my-project"
}

variable "v" {
  default = "x"
}

locals {
  x = 1
}

module "m" {
  source = "./mod"
}

output "o" {
  value = 1
}

resource "google_storage_bucket" "kept" {
  name = "kept"
}

data "google_project" "kept_data" {}

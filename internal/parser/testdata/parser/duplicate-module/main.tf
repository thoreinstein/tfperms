module "first" {
  source = "./mod"
  name   = "first-instance"
}

module "second" {
  source = "./mod"
  name   = "second-instance"
}

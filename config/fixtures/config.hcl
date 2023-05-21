locals {
  upstream = "https://repo-fi.voidlinux.org"
}

dynamic "repository" {
  for_each = [
    "/current",
    "/current/debug",
    "/current/nonfree",
    "/current/multilib",
    "/current/multilib/nonfree",
    "/current/multilib/bootstrap",
    "/current/bootstrap",
  ]
  iterator = path
  content {
    upstream = "${upstream}/${path.value}"
    archs = ["x86_64", "i686", "armv7l", "armv6l"]
  }
}

dynamic "repository" {
  for_each = [
    "/current/musl",
    "/current/musl/debug",
    "/current/musl/nonfree",
    "/current/musl/bootstrap",
  ]
  iterator = path
  content {
    upstream = "${upstream}/${path.value}"
    archs = ["x86_64-musl", "armv7l-musl", "armv6l-musl"]
  }
}

dynamic "repository" {
  for_each = [
    "/current/aarch64",
    "/current/aarch64/debug",
    "/current/aarch64/nonfree",
    "/current/aarch64/bootstrap",
  ]
  iterator = path
  content {
    upstream = "${upstream}/${path.value}"
    archs = ["aarch64", "aarch64-musl"]
  }
}

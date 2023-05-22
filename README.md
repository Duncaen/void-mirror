# void-mirror

Simple daemon that mirrors xbps repositories.

## Configuration

The configuration is loaded from `config.hcl` or a hcl or json file specified using the `-conffile` flag.

Example configuration to mirror all repositories:

```hcl
locals {
  upstream = "https://repo-fi.voidlinux.org"
  glibc = [
    for arch in ["x86_64", "i686", "armv6l", "armv7l"]: [
      {arch = arch, path = "/current"},
      {arch = arch, path = "/current/debug"},
      {arch = arch, path = "/current/nonfree"},
      {arch = arch, path = "/current/bootstrap"},
    ]
  ]
  multilib = [
    {arch = "x86_64", path = "/current/multilib"},
    {arch = "x86_64", path = "/current/multilib/nonfree"},
  ]
  musl = [
    for arch in ["x86_64-musl", "armv6l-musl", "armv7l-musl"]: [
      {arch = arch, path = "/current/musl"},
      {arch = arch, path = "/current/musl/debug"},
      {arch = arch, path = "/current/musl/nonfree"},
      {arch = arch, path = "/current/musl/bootstrap"},
    ]
  ]
  aarch64 = [
    for arch in ["aarch64", "aarch64-musl"]: [
      {arch = arch, path = "/current/aarch64"},
      {arch = arch, path = "/current/aarch64/debug"},
      {arch = arch, path = "/current/aarch64/nonfree"},
      {arch = arch, path = "/current/aarch64/bootstrap"},
    ]
  ]
}

jobs = 8

dynamic "repository" {
  for_each = flatten(concat(glibc, multilib, musl, aarch64))
  iterator = repo
  content {
    upstream = "${upstream}/${repo.value.path}"
    interval = "30s"
    architecture = "${repo.value.arch}"
    destination = "/srv/www/${repo.value.path}"
  }
}
```

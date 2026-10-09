# The service block of github.com/go-authn/bridge, derived from
# packaging/systemd/authn-bridge.service in that repository.
provides = ["bin/authn-bridge"]

service "authn-bridge" {
  description   = "authn-bridge, an OpenID Connect provider in front of a SAML federation"
  documentation = "https://github.com/go-authn/bridge/blob/main/docs/install.md"
  command       = "authn-bridge"
  args          = ["--config", "/etc/authn-bridge"]
  stop-timeout  = "20s"
  state-directory { mode = "0700" }
  runtime-directory { mode = "0700" }
  configuration-directory { mode = "0750" }
}

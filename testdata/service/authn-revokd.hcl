# The service block of github.com/go-authn/revocation, derived from
# packaging/systemd/authn-revokd.service in that repository.
provides = ["bin/authn-revokd"]

service "authn-revokd" {
  description   = "go-authn revocation list agent (SSH KRLs, X.509 CRLs)"
  documentation = "https://github.com/go-authn/revocation"
  command       = "authn-revokd"
  args          = ["-config", "/etc/authn-revokd/revokd.hcl"]
  # sshd ignores a KRL's expiry; only this process fails a lapsed list closed.
  restart = "always"
  # SIGHUP syncs every source now.
  reload = "hup"
  # 0755: sshd reads the KRL written here.
  state-directory { mode = "0755" }
}

# The service block of github.com/go-authn/authnd, derived from
# packaging/systemd/authnd.service in that repository.
provides = ["bin/authnd"]

service "authnd" {
  description   = "authnd LDAP directory and Kerberos KDC"
  documentation = "https://github.com/go-authn/authnd"
  command       = "authnd"
  args          = ["--config", "/etc/authnd"]
  # LDAP 389, LDAPS 636, Kerberos 88.
  capabilities = ["CAP_NET_BIND_SERVICE"]
  # ProcSubset=pid hides /proc/sys, where Go reads net.core.somaxconn: every
  # listener's backlog fell from 4096 to 128.
  proc-subset = "all"
  state-directory { mode = "0700" }
}

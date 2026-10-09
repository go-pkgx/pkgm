package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-pkgx/bottle"
)

// `pkgm service`: run a package's program as a supervised, versioned service.
//
// # ONE TEMPLATE PER SERVICE, THE VERSION IS THE INSTANCE
//
// pkgm writes /etc/systemd/system/<svc>@.service once per service, and the
// instance name is the VERSION:
//
//	ExecStart=<store>/<project>/v%i/bin/<command> …
//
// so authn-bridge@0.20.0.service runs exactly that version, `systemctl status`
// says which one is running, and nothing on the machine has to be believed
// about it: no `current` symlink, no version typed into a unit.
//
// # THE UNIT IS RENDERED FROM DATA
//
// The recipe's `service "<name>" { … }` block (bottle.Service) says what
// differs between daemons; everything else — the hardening — is written here,
// once, for every service. It was derived from three hand-written units
// verified with `systemd-analyze security`: go-authn's authnd, authn-bridge
// and authn-revokd.
//
// # THE FIRST LINE OF EVERY FILE PKGM WRITES SAYS WHOSE IT IS
//
//	# pkgm-service project=<project> store=<store> [previous=<version>]
//
// `pkgm uninstall` reads ONLY that line. A unit names other paths further
// down (and may one day carry an Environment= naming other packages), and a
// search of the whole file is exactly how `uninstall jq` once removed
// oniguruma's program: the stub's LD_LIBRARY_PATH named it.

// The places and programs `pkgm service` touches. Variables so a test can
// point them at a temporary directory and a fake systemd.
var (
	unitDir       = "/etc/systemd/system"
	sysusersDir   = "/etc/sysusers.d"
	systemdRunDir = "/run/systemd/system"
	lookPath      = exec.LookPath
	geteuid       = os.Geteuid
	// runCommand runs a program and returns what it printed, stdout and
	// stderr together.
	runCommand = func(name string, args ...string) (string, error) {
		out, err := exec.Command(name, args...).CombinedOutput()
		return string(out), err
	}
	// settleFor is how long a started instance must STAY active before it
	// is trusted. Type=exec reports success once the program is exec'd; a
	// daemon that then refuses its configuration exits a moment later, and
	// a check made at once would call that a success.
	settleFor   = 5 * time.Second
	settleEvery = 500 * time.Millisecond
	sleepFor    = time.Sleep
	fetchSvc    = bottle.FetchService
	// reachable is checkReachable; a test store sits under a 0700 temporary
	// directory no other account could reach.
	reachable = checkReachable
)

const markerPrefix = "# pkgm-service "

// unitMarker is the parsed first line of a file pkgm wrote.
type unitMarker struct {
	project, store, previous string
}

func (m unitMarker) String() string {
	s := markerPrefix + "project=" + m.project + " store=" + m.store
	if m.previous != "" {
		s += " previous=" + m.previous
	}
	return s
}

// parseMarker reads a marker line as FIELDS. Comparing the project as a whole
// field is what keeps gnu.org/gcc from matching gnu.org/gcc/libstdcxx, the
// trap the stub matcher fell into first.
func parseMarker(line string) (unitMarker, bool) {
	rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), markerPrefix)
	if !ok {
		return unitMarker{}, false
	}
	var m unitMarker
	for _, f := range strings.Fields(rest) {
		k, v, _ := strings.Cut(f, "=")
		switch k {
		case "project":
			m.project = v
		case "store":
			m.store = v
		case "previous":
			m.previous = v
		}
	}
	return m, m.project != "" && m.store != ""
}

// readMarker reads the FIRST LINE of a file and nothing else.
func readMarker(path string) (unitMarker, bool) {
	b, err := readHead(path, 1024)
	if err != nil {
		return unitMarker{}, false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return parseMarker(line)
}

// template is one pkgm-written template unit found on disk.
type template struct {
	svc    string // authn-bridge
	path   string // /etc/systemd/system/authn-bridge@.service
	marker unitMarker
}

func templatePath(svc string) string { return filepath.Join(unitDir, svc+"@.service") }

// pkgmTemplates lists the template units pkgm wrote, by their marker.
func pkgmTemplates() ([]template, error) {
	ents, err := os.ReadDir(unitDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []template
	for _, e := range ents {
		svc, ok := strings.CutSuffix(e.Name(), "@.service")
		if !ok || e.IsDir() {
			continue
		}
		p := filepath.Join(unitDir, e.Name())
		if m, ok := readMarker(p); ok {
			out = append(out, template{svc: svc, path: p, marker: m})
		}
	}
	return out, nil
}

// findTemplate returns the pkgm template for svc, refusing a unit somebody
// else wrote.
func findTemplate(svc string) (template, error) {
	p := templatePath(svc)
	if _, err := os.Stat(p); err != nil {
		return template{}, fmt.Errorf("%s is not a pkgm service: there is no %s", svc, p)
	}
	m, ok := readMarker(p)
	if !ok {
		return template{}, fmt.Errorf("%s was not written by pkgm (its first line is not %q…): it is left alone", p, strings.TrimSpace(markerPrefix))
	}
	return template{svc: svc, path: p, marker: m}, nil
}

// enabledVersions reads, ON DISK, which instances of svc are enabled: the
// <svc>@<version>.service links in the *.wants directories. It needs no
// running systemd, so it answers on a machine where systemctl cannot.
func enabledVersions(svc string) []string {
	links, _ := filepath.Glob(filepath.Join(unitDir, "*.wants", svc+"@*.service"))
	seen := map[string]bool{}
	var out []string
	for _, l := range links {
		v := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(l), svc+"@"), ".service")
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// activeVersions asks systemd which instances of svc are running. A failure
// is returned, never read as "none": a refusal that cannot ask must not
// conclude that nothing is running.
func activeVersions(systemctl, svc string) ([]string, error) {
	out, err := runCommand(systemctl, "list-units", "--all", "--plain", "--no-legend", "--full", svc+"@*.service")
	if err != nil {
		return nil, fmt.Errorf("systemctl list-units: %v: %s", err, strings.TrimSpace(out))
	}
	var vs []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == "●" { // a failed unit is flagged even with --plain
			f = f[1:]
		}
		if len(f) < 3 {
			continue
		}
		v, ok := strings.CutPrefix(f[0], svc+"@")
		if !ok || !strings.HasSuffix(v, ".service") {
			continue
		}
		switch f[2] { // the ACTIVE column
		case "active", "activating", "deactivating", "reloading", "refreshing":
			vs = append(vs, strings.TrimSuffix(v, ".service"))
		}
	}
	sort.Strings(vs)
	return vs, nil
}

// --- the uninstall refusal --------------------------------------------------

// refuseUninstallInUse returns an error if any of the projects is what an
// enabled or running service instance executes. It reads the disk and, if it
// exists, asks systemctl; it never asks the network.
//
// ⛔ ONLY `pkgm uninstall` DELETES A VERSION DIRECTORY. `update` and `pin`
// install beside what is there and remove nothing, which is why this guard
// sits in cmdUninstall alone. TestOnlyUninstallRemovesFromTheStore pins that
// fact, because it can stop being true quietly.
func refuseUninstallInUse(projects []string, store string) error {
	tpls, err := pkgmTemplates()
	if err != nil {
		return fmt.Errorf("cannot read %s to check for services, so nothing was removed: %w", unitDir, err)
	}
	systemctl, _ := lookPath("systemctl")
	var lines, ways []string
	for _, project := range projects {
		for _, t := range tpls {
			if t.marker.project != project || !sameDir(t.marker.store, store) {
				continue
			}
			enabled := enabledVersions(t.svc)
			var active []string
			if systemctl != "" {
				active, err = activeVersions(systemctl, t.svc)
				if err != nil {
					return fmt.Errorf("%s: could not ask systemd whether %s is running (%v), so nothing was removed", project, t.svc, err)
				}
			}
			states := map[string][]string{}
			for _, v := range enabled {
				states[v] = append(states[v], "enabled")
			}
			for _, v := range active {
				states[v] = append(states[v], "active")
			}
			if len(states) == 0 {
				continue
			}
			vs := make([]string, 0, len(states))
			for v := range states {
				vs = append(vs, v)
			}
			sort.Strings(vs)
			for _, v := range vs {
				lines = append(lines, fmt.Sprintf("  %s@%s.service (%s) runs %s",
					t.svc, v, strings.Join(states[v], ", "), filepath.Join(store, project, "v"+v)))
			}
			ways = append(ways, fmt.Sprintf("run \"pkgm service remove %s\" first", t.svc))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to uninstall: a service runs it, and nothing was removed:\n%s\n%s",
		strings.Join(lines, "\n"), strings.Join(ways, "\n"))
}

// sameDir compares two store paths as directories, not as strings: /opt/pkgx
// and /opt/pkgx/ are one store, and so are a path and a symlink to it.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

// --- rendering ----------------------------------------------------------------

// renderUnit writes the template unit for a service.
//
// The path in ExecStart comes from bottle.PrefixOf with `%i` for the version,
// so the convention <store>/<project>/v<version> is spelled in one place, and
// no version is ever typed into a unit.
func renderUnit(s *bottle.Service, m unitMarker) string {
	prefix := bottle.PrefixOf(m.project, []bottle.Resolved{{Project: m.project, Version: bottle.Ver{Raw: "%i"}}}, m.store)
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("%s", m)
	w("#")
	w("# Written by `pkgm service install` from the service block of the")
	w("# %s recipe. pkgm rewrites this file: put local changes", m.project)
	w("# in a drop-in instead (systemctl edit %s@.service).", s.Name)
	w("#")
	w("# The instance is the VERSION: %s@<version>.service runs", s.Name)
	w("# %s/v<version>/bin/%s.", filepath.Join(m.store, m.project), s.Command)
	w("")
	w("[Unit]")
	w("Description=%s", unitEscape(s.Description))
	if s.Documentation != "" {
		w("Documentation=%s", unitEscape(s.Documentation))
	}
	w("Wants=network-online.target")
	w("After=network-online.target")
	w("")
	w("[Service]")
	w("Type=exec")
	w("ExecStart=%s", strings.Join(append([]string{filepath.Join(prefix, "bin", s.Command)}, s.Args...), " "))
	if s.Reload == "hup" {
		w("ExecReload=/bin/kill -HUP $MAINPID")
	}
	w("Restart=%s", s.Restart)
	w("RestartSec=5s")
	if s.StopTimeout != "" {
		w("TimeoutStopSec=%s", s.StopTimeout)
	}
	w("")
	w("User=%s", s.User)
	w("Group=%s", s.User)
	w("UMask=0077")
	dirs := []struct {
		key string
		d   *bottle.ServiceDirectory
	}{
		{"StateDirectory", s.StateDirectory},
		{"RuntimeDirectory", s.RuntimeDirectory},
		{"ConfigurationDirectory", s.ConfigurationDirectory},
	}
	for _, d := range dirs {
		if d.d != nil {
			w("%s=%s", d.key, s.Name)
			w("%sMode=%s", d.key, d.d.Mode)
		}
	}
	w("")
	w("NoNewPrivileges=yes")
	caps := strings.Join(s.Capabilities, " ")
	w("CapabilityBoundingSet=%s", caps)
	w("AmbientCapabilities=%s", caps)
	w("RestrictSUIDSGID=yes")
	// DERIVED, not declared: a capability does not apply to the host's
	// network from inside a user namespace (measured with authnd: "listen
	// tcp 0.0.0.0:389: bind: permission denied").
	if len(s.Capabilities) == 0 {
		w("PrivateUsers=yes")
	}
	w("")
	for _, l := range []string{
		"ProtectSystem=strict", "ProtectHome=yes", "PrivateTmp=yes", "PrivateDevices=yes",
		"DevicePolicy=closed", "ProtectProc=invisible",
	} {
		w("%s", l)
	}
	if s.ProcSubset == "pid" {
		w("ProcSubset=pid")
	}
	for _, l := range []string{
		"RemoveIPC=yes", "PrivateIPC=yes", "",
		"ProtectKernelTunables=yes", "ProtectKernelModules=yes", "ProtectKernelLogs=yes",
		"ProtectControlGroups=yes", "ProtectClock=yes", "ProtectHostname=yes",
		"LockPersonality=yes", "MemoryDenyWriteExecute=yes", "RestrictRealtime=yes",
		"RestrictNamespaces=yes", "",
		"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
		"SystemCallArchitectures=native",
		"SystemCallFilter=@system-service",
		"SystemCallFilter=~@privileged @resources",
		"SystemCallErrorNumber=EPERM", "",
		"[Install]",
		"WantedBy=multi-user.target",
	} {
		w("%s", l)
	}
	return b.String()
}

// unitEscape makes prose safe in a unit value: `%` is a specifier there.
// Newlines and control characters cannot reach this point; bottle refuses
// them when it reads the block.
func unitEscape(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// renderSysusers writes the systemd-sysusers line for the service's account.
func renderSysusers(s *bottle.Service, m unitMarker) string {
	home := "-"
	if s.StateDirectory != nil {
		home = "/var/lib/" + s.Name
	}
	return fmt.Sprintf("%s\n#\n# The account %s@.service runs as, written by `pkgm service install`.\n"+
		"# A static account, not DynamicUser=: its files must keep one owner across\n"+
		"# versions, reinstalls and restores.\n#\n"+
		"#Type Name ID GECOS Home Shell\nu %s - \"%s\" %s -\n",
		unitMarker{project: m.project, store: m.store}, s.Name, s.User, s.Description, home)
}

// --- preconditions --------------------------------------------------------------

// needSystemd refuses, in one sentence, on a machine with no running systemd.
// A FROM-scratch image is pkgm's core use case and has no service manager; the
// alternative was "exec: systemctl: executable file not found", which names
// a symptom and not the reason.
func needSystemd(cmd string) (string, error) {
	if st, err := os.Stat(systemdRunDir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("pkgm service %s needs systemd, and this system is not running it (%s does not exist): "+
			"a container or FROM-scratch image has no service manager, so run the program directly, e.g. with `pkgm run`", cmd, systemdRunDir)
	}
	systemctl, err := lookPath("systemctl")
	if err != nil {
		return "", fmt.Errorf("pkgm service %s needs systemctl, and systemd is running but systemctl is not on PATH", cmd)
	}
	if geteuid() != 0 {
		return "", fmt.Errorf("pkgm service %s writes %s and runs systemctl, so it must run as root", cmd, unitDir)
	}
	return systemctl, nil
}

// protectedStoreRoots are directories the rendered hardening hides from the
// service (ProtectHome=yes, PrivateTmp=yes). A store under one of them would
// install fine and then fail at start with a "No such file or directory" for
// a program that is plainly there.
var protectedStoreRoots = []string{"/home", "/root", "/run/user", "/tmp", "/var/tmp"}

// storePathRE is the alphabet a store path may use in a unit without escaping.
var storePathRE = regexp.MustCompile(`^/[A-Za-z0-9_./+-]*$`)

func checkStore(store string) error {
	if !storePathRE.MatchString(store) || strings.Contains(store, "%") {
		return fmt.Errorf("the store %q cannot be written into a unit: it must be an absolute path of letters, digits and _ . / + -", store)
	}
	for _, r := range protectedStoreRoots {
		if store == r || strings.HasPrefix(store, r+"/") {
			return fmt.Errorf("the store %s is under %s, which the service's sandbox hides (ProtectHome=, PrivateTmp=): "+
				"set PKGX_DIR to a directory outside /home, /root, /run/user and /tmp, e.g. /opt/pkgx", store, r)
		}
	}
	return nil
}

// checkReachable refuses a program the service account could not run: every
// directory from / down to it must be searchable by others, and the program
// executable by them.
func checkReachable(bin, user string) error {
	for p := bin; ; p = filepath.Dir(p) {
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		if st.Mode().Perm()&0o001 == 0 {
			return fmt.Errorf("%s runs as %s, which cannot reach %s (mode %04o)", bin, user, p, st.Mode().Perm())
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

// checkProgram refuses what a template unit cannot run: a program the recipe
// does not provide, or one that needs a dynamic loader. A unit is shared by
// every version, so it cannot carry one version's LD_LIBRARY_PATH the way a
// stub does; a static program needs none.
func checkProgram(project string, s *bottle.Service, provides []string, bin string) error {
	declared := false
	for _, p := range provides {
		declared = declared || p == "bin/"+s.Command
	}
	if !declared {
		return fmt.Errorf("%s: service %s runs %q, which the recipe does not provide (provides: %s)",
			project, s.Name, s.Command, strings.Join(provides, ", "))
	}
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("%s: %s is not in the installed bottle", project, bin)
	}
	interp, err := bottle.ELFInterp(bin)
	if err != nil {
		return fmt.Errorf("%s: %s is not an ELF program (%v); pkgm service runs statically linked programs only", project, bin, err)
	}
	if interp != "" {
		return fmt.Errorf("%s: %s is dynamically linked (interpreter %s); pkgm service runs statically linked programs only, "+
			"because one unit serves every version and cannot carry one version's library path", project, bin, interp)
	}
	return nil
}

// versionRE is what a version must look like to be a unit instance name
// without escaping. systemd escapes anything else (a `+` becomes \x2b), and
// an instance that does not read back as the version breaks every lookup.
var versionRE = regexp.MustCompile(`^[0-9][A-Za-z0-9._-]*$`)

// serviceConstraint reads <pkg>[@version]. A bare version is EXACT, as `pin`
// takes it: the version is the name of what will run, so "0.20" must not
// quietly mean 0.20.7. An operator (^ ~ < > = *) is a constraint.
func serviceConstraint(arg string) (project, constraint string) {
	project, constraint = parseReq(arg, false)
	if constraint != "*" && !strings.ContainsAny(constraint[:1], "^~<>=*") {
		constraint = "=" + constraint
	}
	return project, constraint
}

// ensureInstalled resolves and installs the closure, returning the version.
func ensureInstalled(project, constraint, store string) (string, error) {
	closure, err := bottle.ResolveClosure(map[string]string{project: constraint})
	if err != nil {
		return "", err
	}
	for _, r := range closure {
		fresh, err := bottle.Install(r, store)
		if err != nil {
			return "", fmt.Errorf("%s: %w", r.Project, err)
		}
		state := "cached"
		if fresh {
			state = "installed"
		}
		fmt.Printf("  %-9s %s v%s\n", state, r.Project, r.Version.Raw)
	}
	prefix := bottle.PrefixOf(project, closure, store)
	return strings.TrimPrefix(filepath.Base(prefix), "v"), nil
}

// prepare fetches the service block, installs the version and checks that the
// unit pkgm would write can run it. It returns the service, the version and
// the template that would be written.
func prepare(project, constraint, store string) (*bottle.Service, string, error) {
	if err := checkStore(store); err != nil {
		return nil, "", err
	}
	s, err := fetchSvc(project)
	if err != nil {
		return nil, "", err
	}
	if s == nil {
		return nil, "", fmt.Errorf("%s declares no service: its recipe has no `service` block", project)
	}
	v, err := ensureInstalled(project, constraint, store)
	if err != nil {
		return nil, "", err
	}
	if !versionRE.MatchString(v) {
		return nil, "", fmt.Errorf("%s: version %q cannot be a unit instance name", project, v)
	}
	_, provides, err := bottle.FetchMeta(project)
	if err != nil {
		return nil, "", err
	}
	bin := filepath.Join(store, project, "v"+v, "bin", s.Command)
	if err := checkProgram(project, s, provides, bin); err != nil {
		return nil, "", err
	}
	if err := reachable(bin, s.User); err != nil {
		return nil, "", err
	}
	return s, v, nil
}

// writeOwned writes a file pkgm owns, refusing to replace one it does not.
func writeOwned(path, content string) (changed bool, err error) {
	old, err := os.ReadFile(path)
	switch {
	case err == nil:
		line, _, _ := strings.Cut(string(old), "\n")
		if _, ok := parseMarker(line); !ok {
			return false, fmt.Errorf("%s exists and was not written by pkgm: it is left alone", path)
		}
		if string(old) == content {
			return false, nil
		}
	case !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp := path + ".pkgm-tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// --- commands -------------------------------------------------------------------

func cmdService(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("service: install|switch|rollback|list|remove (try --help)")
	}
	sub, rest := args[0], args[1:]
	one := func() (string, error) {
		if len(rest) != 1 {
			return "", fmt.Errorf("pkgm service %s takes exactly one argument", sub)
		}
		return rest[0], nil
	}
	switch sub {
	case "install":
		a, err := one()
		if err != nil {
			return err
		}
		return serviceInstall(a, stdout)
	case "switch":
		a, err := one()
		if err != nil {
			return err
		}
		return serviceSwitch(a, stdout)
	case "rollback":
		a, err := one()
		if err != nil {
			return err
		}
		return serviceRollback(a, stdout)
	case "list", "ls":
		return serviceList(stdout)
	case "remove", "rm":
		a, err := one()
		if err != nil {
			return err
		}
		return serviceRemove(a, stdout)
	default:
		return fmt.Errorf("unknown service command %q: install|switch|rollback|list|remove", sub)
	}
}

func systemctlDo(prog string, args ...string) error {
	out, err := runCommand(prog, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", filepath.Base(prog), strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return nil
}

// settled reports whether an instance became active and STAYED active for
// settleFor. It returns the state it last saw.
func settled(systemctl, unit string) (bool, string) {
	deadline := settleFor
	for {
		out, _ := runCommand(systemctl, "is-active", unit)
		state := strings.TrimSpace(out)
		if state != "active" {
			return false, state
		}
		if deadline <= 0 {
			return true, state
		}
		sleepFor(settleEvery)
		deadline -= settleEvery
	}
}

func serviceInstall(arg string, w io.Writer) error {
	systemctl, err := needSystemd("install")
	if err != nil {
		return err
	}
	project, constraint := serviceConstraint(arg)
	store := bottle.Dir()
	s, v, err := prepare(project, constraint, store)
	if err != nil {
		return err
	}
	if other := otherVersions(enabledVersions(s.Name), v); len(other) > 0 {
		return fmt.Errorf("%s already runs %s: use `pkgm service switch %s@%s` to change the version",
			s.Name, strings.Join(other, ", "), project, v)
	}
	m := unitMarker{project: project, store: store}
	if t, err := findTemplate(s.Name); err == nil {
		m.previous = t.marker.previous
		if t.marker.project != project {
			return fmt.Errorf("%s is the service of %s, not of %s", s.Name, t.marker.project, project)
		}
	} else if _, statErr := os.Stat(templatePath(s.Name)); statErr == nil {
		return err
	}
	if _, err := writeOwned(filepath.Join(sysusersDir, "pkgm-"+s.Name+".conf"), renderSysusers(s, m)); err != nil {
		return err
	}
	sysusers, err := lookPath("systemd-sysusers")
	if err != nil {
		return fmt.Errorf("pkgm service install needs systemd-sysusers to create the account %s, and it is not on PATH", s.User)
	}
	if err := systemctlDo(sysusers, filepath.Join(sysusersDir, "pkgm-"+s.Name+".conf")); err != nil {
		return err
	}
	changed, err := writeOwned(templatePath(s.Name), renderUnit(s, m))
	if err != nil {
		return err
	}
	if err := systemctlDo(systemctl, "daemon-reload"); err != nil {
		return err
	}
	unit := s.Name + "@" + v + ".service"
	wasActive, _ := runCommand(systemctl, "is-active", unit)
	if err := systemctlDo(systemctl, "enable", "--now", unit); err != nil {
		return err
	}
	if ok, state := settled(systemctl, unit); !ok {
		return fmt.Errorf("%s was started and did not stay active (%s): see `journalctl -u %s`", unit, state, unit)
	}
	fmt.Fprintf(w, "%s: %s is active, running %s\n", s.Name, unit, filepath.Join(store, project, "v"+v, "bin", s.Command))
	if changed && strings.TrimSpace(wasActive) == "active" {
		fmt.Fprintf(w, "the unit changed while %s was running: `systemctl restart %s` applies it\n", unit, unit)
	}
	return nil
}

func otherVersions(vs []string, v string) []string {
	var out []string
	for _, x := range vs {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// currentVersion is the one enabled version of a service.
func currentVersion(svc string) (string, error) {
	vs := enabledVersions(svc)
	switch len(vs) {
	case 0:
		return "", fmt.Errorf("%s has no enabled version: use `pkgm service install`", svc)
	case 1:
		return vs[0], nil
	default:
		return "", fmt.Errorf("%s has several enabled versions (%s): disable all but one with systemctl", svc, strings.Join(vs, ", "))
	}
}

// handover moves a service from one version to another.
//
// ⛔ NOT "START THE NEW ONE, THEN STOP THE OLD". That order keeps a service up
// only if two versions can run at once, and a daemon that listens cannot:
// authn-bridge@0.20.0 started beside 0.19.4 dies on "bind: address already in
// use" (measured in the VM), so every switch would have been refused. So the
// old instance stops, the new one starts and must stay active for settleFor,
// and if it does not, it is stopped and the OLD ONE IS STARTED AGAIN: the
// service ends up running either the new version or the old one, never
// neither.
func handover(systemctl, svc, from, to string) error {
	oldU, newU := svc+"@"+from+".service", svc+"@"+to+".service"
	if err := systemctlDo(systemctl, "stop", oldU); err != nil {
		return err
	}
	if err := systemctlDo(systemctl, "start", newU); err == nil {
		if ok, _ := settled(systemctl, newU); ok {
			if err := systemctlDo(systemctl, "enable", newU); err != nil {
				return err
			}
			if err := systemctlDo(systemctl, "disable", oldU); err != nil {
				return err
			}
			return nil
		}
	}
	_, state := settled(systemctl, newU)
	_ = systemctlDo(systemctl, "stop", newU)
	if err := systemctlDo(systemctl, "start", oldU); err != nil {
		return fmt.Errorf("%s did not stay active (%s), and %s could not be started again: %v", newU, state, oldU, err)
	}
	return fmt.Errorf("%s did not stay active (%s), so %s is running again: see `journalctl -u %s`", newU, state, oldU, newU)
}

// setPrevious rewrites the marker line of a template, keeping the rest.
func setPrevious(t template, previous string) error {
	b, err := os.ReadFile(t.path)
	if err != nil {
		return err
	}
	_, rest, _ := strings.Cut(string(b), "\n")
	m := t.marker
	m.previous = previous
	_, err = writeOwned(t.path, m.String()+"\n"+rest)
	return err
}

func serviceSwitch(arg string, w io.Writer) error {
	systemctl, err := needSystemd("switch")
	if err != nil {
		return err
	}
	project, constraint := serviceConstraint(arg)
	if constraint == "*" {
		return fmt.Errorf("pkgm service switch needs a version: %s@<version>", project)
	}
	store := bottle.Dir()
	s, v, err := prepare(project, constraint, store)
	if err != nil {
		return err
	}
	t, err := findTemplate(s.Name)
	if err != nil {
		return err
	}
	from, err := currentVersion(s.Name)
	if err != nil {
		return err
	}
	if from == v {
		return fmt.Errorf("%s already runs %s", s.Name, v)
	}
	m := unitMarker{project: project, store: store, previous: t.marker.previous}
	if _, err := writeOwned(t.path, renderUnit(s, m)); err != nil {
		return err
	}
	if err := systemctlDo(systemctl, "daemon-reload"); err != nil {
		return err
	}
	if err := handover(systemctl, s.Name, from, v); err != nil {
		return err
	}
	t.marker = m
	if err := setPrevious(t, from); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: %s → %s; %s@%s.service is active (`pkgm service rollback %s` returns to %s)\n",
		s.Name, from, v, s.Name, v, s.Name, from)
	return systemctlDo(systemctl, "daemon-reload")
}

// serviceRollback returns a service to the version it ran before the last
// switch. It works OFFLINE: the version is already in the store and the unit
// already on disk, so nothing is resolved and nothing is fetched.
func serviceRollback(svc string, w io.Writer) error {
	systemctl, err := needSystemd("rollback")
	if err != nil {
		return err
	}
	t, err := findTemplate(svc)
	if err != nil {
		return err
	}
	to := t.marker.previous
	if to == "" {
		return fmt.Errorf("%s has no previous version to return to: it has not been switched", svc)
	}
	if !versionRE.MatchString(to) {
		return fmt.Errorf("%s: previous version %q in %s is not a version", svc, to, t.path)
	}
	if st, err := os.Stat(filepath.Join(t.marker.store, t.marker.project, "v"+to)); err != nil || !st.IsDir() {
		return fmt.Errorf("%s: %s is no longer in the store (%s): `pkgm service switch %s@%s` installs it again",
			svc, to, filepath.Join(t.marker.store, t.marker.project), t.marker.project, to)
	}
	from, err := currentVersion(svc)
	if err != nil {
		return err
	}
	if err := handover(systemctl, svc, from, to); err != nil {
		return err
	}
	if err := setPrevious(t, from); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: rolled back %s → %s; %s@%s.service is active\n", svc, from, to, svc, to)
	return systemctlDo(systemctl, "daemon-reload")
}

// serviceList says which version each service runs. It reads the disk, so it
// answers on a machine with no systemd too; only the active state needs it.
func serviceList(w io.Writer) error {
	tpls, err := pkgmTemplates()
	if err != nil {
		return err
	}
	if len(tpls) == 0 {
		fmt.Fprintf(w, "no pkgm services in %s\n", unitDir)
		return nil
	}
	systemctl := ""
	if st, err := os.Stat(systemdRunDir); err == nil && st.IsDir() {
		systemctl, _ = lookPath("systemctl")
	}
	for _, t := range tpls {
		enabled := enabledVersions(t.svc)
		var active []string
		activeKnown := systemctl != ""
		if activeKnown {
			if active, err = activeVersions(systemctl, t.svc); err != nil {
				activeKnown = false
			}
		}
		vs := map[string]bool{}
		for _, v := range append(append([]string{}, enabled...), active...) {
			vs[v] = true
		}
		names := make([]string, 0, len(vs))
		for v := range vs {
			names = append(names, v)
		}
		sort.Strings(names)
		var parts []string
		for _, v := range names {
			var st []string
			if contains(enabled, v) {
				st = append(st, "enabled")
			}
			switch {
			case !activeKnown:
				st = append(st, "active: unknown")
			case contains(active, v):
				st = append(st, "active")
			default:
				st = append(st, "inactive")
			}
			parts = append(parts, fmt.Sprintf("%s (%s)", v, strings.Join(st, ", ")))
		}
		running := "nothing enabled or running"
		if len(parts) > 0 {
			running = strings.Join(parts, "; ")
		}
		line := fmt.Sprintf("%s\t%s\t%s", t.svc, t.marker.project, running)
		if t.marker.previous != "" {
			line += "\tprevious " + t.marker.previous
		}
		fmt.Fprintln(w, line)
	}
	return nil
}

func contains(vs []string, v string) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

// serviceRemove stops and disables every instance of a service and deletes its
// template. The package stays installed (that is `pkgm uninstall`), and so do
// the account and the state, configuration and runtime directories, which
// belong to the site, not to a version.
func serviceRemove(svc string, w io.Writer) error {
	systemctl, err := needSystemd("remove")
	if err != nil {
		return err
	}
	t, err := findTemplate(svc)
	if err != nil {
		return err
	}
	active, err := activeVersions(systemctl, svc)
	if err != nil {
		return err
	}
	vs := map[string]bool{}
	for _, v := range append(enabledVersions(svc), active...) {
		vs[v] = true
	}
	names := make([]string, 0, len(vs))
	for v := range vs {
		names = append(names, v)
	}
	sort.Strings(names)
	for _, v := range names {
		unit := svc + "@" + v + ".service"
		if err := systemctlDo(systemctl, "disable", "--now", unit); err != nil {
			return err
		}
		fmt.Fprintf(w, "  stopped and disabled %s\n", unit)
	}
	if err := os.Remove(t.path); err != nil {
		return err
	}
	if err := systemctlDo(systemctl, "daemon-reload"); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: removed %s; kept the account and its directories (/etc/sysusers.d/pkgm-%s.conf, /var/lib/%s, /etc/%s); "+
		"`pkgm uninstall %s` may now remove the package\n", svc, t.path, svc, svc, svc, t.marker.project)
	return nil
}

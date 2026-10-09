package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-pkgx/bottle"
)

var update = flag.Bool("update", false, "rewrite the golden units in testdata/service")

// --- fixtures -----------------------------------------------------------------

// elfBytes is a minimal ELF64 file: statically linked when interp is "",
// dynamically linked (one PT_INTERP) otherwise. Enough for debug/elf, which is
// all bottle.ELFInterp reads.
func elfBytes(interp string) []byte {
	var b bytes.Buffer
	le := binary.LittleEndian
	b.Write([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0})
	b.Write(make([]byte, 8))
	phnum, phoff := uint16(0), uint64(0)
	if interp != "" {
		phnum, phoff = 1, 64
	}
	_ = binary.Write(&b, le, uint16(2))    // ET_EXEC
	_ = binary.Write(&b, le, uint16(0xb7)) // aarch64
	_ = binary.Write(&b, le, uint32(1))
	_ = binary.Write(&b, le, uint64(0)) // entry
	_ = binary.Write(&b, le, phoff)
	_ = binary.Write(&b, le, uint64(0)) // shoff
	_ = binary.Write(&b, le, uint32(0))
	_ = binary.Write(&b, le, uint16(64)) // ehsize
	_ = binary.Write(&b, le, uint16(56)) // phentsize
	_ = binary.Write(&b, le, phnum)
	_ = binary.Write(&b, le, uint16(64))
	_ = binary.Write(&b, le, uint16(0))
	_ = binary.Write(&b, le, uint16(0))
	if interp != "" {
		s := append([]byte(interp), 0)
		_ = binary.Write(&b, le, uint32(3)) // PT_INTERP
		_ = binary.Write(&b, le, uint32(4))
		_ = binary.Write(&b, le, uint64(120))
		_ = binary.Write(&b, le, uint64(0))
		_ = binary.Write(&b, le, uint64(0))
		_ = binary.Write(&b, le, uint64(len(s)))
		_ = binary.Write(&b, le, uint64(len(s)))
		_ = binary.Write(&b, le, uint64(1))
		b.Write(s)
	}
	return b.Bytes()
}

func serviceFixture(t *testing.T, name string) *bottle.Service {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "service", name+".hcl"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := bottle.ParseService(b, name+".hcl")
	if err != nil || s == nil {
		t.Fatalf("%s: %v", name, err)
	}
	return s
}

// fakeSystemd is a systemd that keeps its state in memory and its enablement
// ON DISK, as the real one does: `enable` makes the *.wants link that
// enabledVersions reads, so the refusal is tested against the files it reads
// in production rather than against the fake's own bookkeeping.
type fakeSystemd struct {
	t       *testing.T
	active  map[string]bool
	failing map[string]bool // units that start and then die
	listErr bool
	calls   []string
}

func (f *fakeSystemd) wants(unit string) string {
	return filepath.Join(unitDir, "multi-user.target.wants", unit)
}

func (f *fakeSystemd) run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, filepath.Base(name)+" "+strings.Join(args, " "))
	if filepath.Base(name) != "systemctl" {
		return "", nil
	}
	now := len(args) > 1 && args[1] == "--now"
	unit := args[len(args)-1]
	switch args[0] {
	case "daemon-reload":
	case "enable":
		_ = os.MkdirAll(filepath.Dir(f.wants(unit)), 0o755)
		_ = os.Symlink("../"+strings.SplitN(unit, "@", 2)[0]+"@.service", f.wants(unit))
		if now {
			f.active[unit] = !f.failing[unit]
		}
	case "disable":
		_ = os.Remove(f.wants(unit))
		if now {
			f.active[unit] = false
		}
	case "start":
		f.active[unit] = !f.failing[unit]
	case "stop":
		f.active[unit] = false
	case "is-active":
		if f.active[unit] {
			return "active\n", nil
		}
		return "failed\n", errors.New("exit status 3")
	case "list-units":
		if f.listErr {
			return "Failed to connect to bus: No such file or directory\n", errors.New("exit status 1")
		}
		var b strings.Builder
		var units []string
		for u, on := range f.active {
			if on {
				units = append(units, u)
			}
		}
		sort.Strings(units)
		for _, u := range units {
			fmt.Fprintf(&b, "%s loaded active running something\n", u)
		}
		return b.String(), nil
	default:
		f.t.Fatalf("unexpected systemctl %v", args)
	}
	return "", nil
}

// withSystemd points every seam at a temporary root and a fake systemd.
func withSystemd(t *testing.T) *fakeSystemd {
	t.Helper()
	root := t.TempDir()
	f := &fakeSystemd{t: t, active: map[string]bool{}, failing: map[string]bool{}}
	oUnit, oSys, oRun, oLook, oEuid, oCmd, oSettle, oSleep, oFetch, oProt, oReach :=
		unitDir, sysusersDir, systemdRunDir, lookPath, geteuid, runCommand, settleFor, sleepFor, fetchSvc, protectedStoreRoots, reachable
	t.Cleanup(func() {
		unitDir, sysusersDir, systemdRunDir, lookPath, geteuid, runCommand, settleFor, sleepFor, fetchSvc, protectedStoreRoots, reachable =
			oUnit, oSys, oRun, oLook, oEuid, oCmd, oSettle, oSleep, oFetch, oProt, oReach
	})
	unitDir = filepath.Join(root, "etc/systemd/system")
	sysusersDir = filepath.Join(root, "etc/sysusers.d")
	systemdRunDir = filepath.Join(root, "run/systemd/system")
	for _, d := range []string{unitDir, sysusersDir, systemdRunDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lookPath = func(name string) (string, error) { return "/fake/" + name, nil }
	geteuid = func() int { return 0 }
	runCommand = f.run
	settleFor, sleepFor = time.Second, func(time.Duration) {}
	// The temporary store is under /tmp on a Linux runner, which the real
	// sandbox hides; checkStore is tested on its own below.
	protectedStoreRoots = nil
	reachable = func(string, string) error { return nil }
	return f
}

const bridge = "github.com/go-authn/bridge"

// withBridge serves two versions of a static authn-bridge and its service
// block, and returns the store.
func withBridge(t *testing.T) string {
	t.Helper()
	t.Cleanup(fakeServer(t, map[string]fakePkg{
		bridge: {
			versions: []string{"0.19.4", "0.20.0"},
			yaml:     "provides:\n  - bin/authn-bridge\n",
			files:    map[string]string{"bin/authn-bridge": string(elfBytes(""))},
		},
	}))
	s := serviceFixture(t, "authn-bridge")
	fetchSvc = func(p string) (*bottle.Service, error) {
		if p == bridge {
			return s, nil
		}
		return nil, nil
	}
	store := t.TempDir()
	t.Setenv("PKGX_DIR", store)
	return store
}

// tree lists every file and link under the given roots, with its size, so a
// refusal can be shown to have touched nothing at all.
func tree(t *testing.T, roots ...string) string {
	t.Helper()
	var out []string
	for _, r := range roots {
		_ = filepath.WalkDir(r, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			info, _ := d.Info()
			out = append(out, fmt.Sprintf("%s %v %d", p, info.Mode().Type(), info.Size()))
			return nil
		})
	}
	return strings.Join(out, "\n")
}

// --- golden units -------------------------------------------------------------------

// The three units, rendered for a store at /opt/pkgx, compared byte for byte.
// Each golden was read against the daemon's hand-written reference unit; the
// differences are listed in the README.
func TestRenderedUnitsAreTheGoldens(t *testing.T) {
	for svc, project := range map[string]string{
		"authnd":       "github.com/go-authn/authnd",
		"authn-bridge": "github.com/go-authn/bridge",
		"authn-revokd": "github.com/go-authn/revocation",
	} {
		s := serviceFixture(t, svc)
		m := unitMarker{project: project, store: "/opt/pkgx"}
		for file, got := range map[string]string{
			svc + "@.service": renderUnit(s, m),
			svc + ".sysusers": renderSysusers(s, m),
		} {
			golden := filepath.Join("testdata", "service", file)
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("%s differs from its golden:\n%s", file, got)
			}
		}
	}
}

// What is DERIVED rather than declared, and what varies, checked as
// properties so that a golden regenerated carelessly cannot hide them.
func TestRenderedUnitProperties(t *testing.T) {
	m := unitMarker{project: "github.com/go-authn/authnd", store: "/opt/pkgx"}
	authnd := renderUnit(serviceFixture(t, "authnd"), m)
	bridgeU := renderUnit(serviceFixture(t, "authn-bridge"), unitMarker{project: bridge, store: "/opt/pkgx"})
	revokd := renderUnit(serviceFixture(t, "authn-revokd"), unitMarker{project: "github.com/go-authn/revocation", store: "/opt/pkgx"})
	checks := []struct {
		unit, line string
		want       bool
	}{
		{authnd, "ExecStart=/opt/pkgx/github.com/go-authn/authnd/v%i/bin/authnd --config /etc/authnd", true},
		{authnd, "AmbientCapabilities=CAP_NET_BIND_SERVICE", true},
		{authnd, "CapabilityBoundingSet=CAP_NET_BIND_SERVICE", true},
		{authnd, "PrivateUsers=yes", false}, // a capability does not apply in a user namespace
		{authnd, "ProcSubset=pid", false},   // it hides somaxconn
		{authnd, "StateDirectoryMode=0700", true},
		{authnd, "ExecReload=", false},
		{bridgeU, "PrivateUsers=yes", true},
		{bridgeU, "CapabilityBoundingSet=\n", true},
		{bridgeU, "ProcSubset=pid", true},
		{bridgeU, "RuntimeDirectoryMode=0700", true},
		{bridgeU, "ConfigurationDirectoryMode=0750", true},
		{bridgeU, "TimeoutStopSec=20s", true},
		{bridgeU, "Restart=on-failure", true},
		{revokd, "Restart=always", true},
		{revokd, "ExecReload=/bin/kill -HUP $MAINPID", true},
		{revokd, "StateDirectoryMode=0755", true},
		{revokd, "ExecStart=/opt/pkgx/github.com/go-authn/revocation/v%i/bin/authn-revokd -config /etc/authn-revokd/revokd.hcl", true},
	}
	for _, c := range checks {
		if strings.Contains(c.unit, c.line) != c.want {
			t.Errorf("%q present=%v, want %v in\n%s", c.line, !c.want, c.want, c.unit)
		}
	}
	if !strings.HasPrefix(authnd, "# pkgm-service project=github.com/go-authn/authnd store=/opt/pkgx\n") {
		t.Errorf("the marker is not the first line:\n%s", authnd)
	}
	s := serviceFixture(t, "authnd")
	s.Description = "100% LDAP"
	if u := renderUnit(s, m); !strings.Contains(u, "Description=100%% LDAP") {
		t.Errorf("a %% in prose is a systemd specifier and must be doubled:\n%s", u)
	}
}

// --- the lifecycle --------------------------------------------------------------------

func TestServiceLifecycle(t *testing.T) {
	sd := withSystemd(t)
	store := withBridge(t)
	prefix := t.TempDir()
	t.Setenv("PKGM_PREFIX", prefix)
	var out bytes.Buffer

	if err := cmdService([]string{"install", bridge + "@0.19.4"}, &out); err != nil {
		t.Fatal(err)
	}
	tpl := filepath.Join(unitDir, "authn-bridge@.service")
	b, err := os.ReadFile(tpl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ExecStart="+store+"/"+bridge+"/v%i/bin/authn-bridge --config /etc/authn-bridge\n") {
		t.Errorf("the template does not run the store's binary by instance:\n%s", b)
	}
	if !sd.active["authn-bridge@0.19.4.service"] || len(enabledVersions("authn-bridge")) != 1 {
		t.Fatalf("after install: active=%v enabled=%v", sd.active, enabledVersions("authn-bridge"))
	}
	if su, err := os.ReadFile(filepath.Join(sysusersDir, "pkgm-authn-bridge.conf")); err != nil ||
		!strings.Contains(string(su), "\nu authn-bridge - \"authn-bridge, an OpenID Connect provider in front of a SAML federation\" /var/lib/authn-bridge -\n") {
		t.Errorf("sysusers: %s %v", su, err)
	}
	if !contains(sd.calls, "systemd-sysusers "+filepath.Join(sysusersDir, "pkgm-authn-bridge.conf")) {
		t.Errorf("systemd-sysusers was not run: %v", sd.calls)
	}
	if !strings.Contains(out.String(), "authn-bridge@0.19.4.service is active") {
		t.Errorf("install output: %s", out.String())
	}

	// A second version is a switch, not a second install.
	if err := cmdService([]string{"install", bridge + "@0.20.0"}, &out); err == nil || !strings.Contains(err.Error(), "already runs 0.19.4") {
		t.Errorf("install of another version: %v", err)
	}
	// Installing the same version again is a refresh.
	if err := cmdService([]string{"install", bridge + "@0.19.4"}, &out); err != nil {
		t.Errorf("reinstall: %v", err)
	}

	out.Reset()
	if err := cmdService([]string{"switch", bridge + "@0.20.0"}, &out); err != nil {
		t.Fatal(err)
	}
	if !sd.active["authn-bridge@0.20.0.service"] || sd.active["authn-bridge@0.19.4.service"] {
		t.Errorf("after switch: %v", sd.active)
	}
	if got := enabledVersions("authn-bridge"); strings.Join(got, ",") != "0.20.0" {
		t.Errorf("enabled after switch: %v", got)
	}
	if m, _ := readMarker(tpl); m.previous != "0.19.4" {
		t.Errorf("previous after switch: %+v", m)
	}
	if err := cmdService([]string{"switch", bridge + "@0.20.0"}, &out); err == nil || !strings.Contains(err.Error(), "already runs 0.20.0") {
		t.Errorf("switch to the running version: %v", err)
	}

	out.Reset()
	if err := serviceList(&out); err != nil {
		t.Fatal(err)
	}
	if want := "authn-bridge\t" + bridge + "\t0.20.0 (enabled, active)\tprevious 0.19.4\n"; out.String() != want {
		t.Errorf("list:\n got %q\nwant %q", out.String(), want)
	}

	out.Reset()
	if err := cmdService([]string{"rollback", "authn-bridge"}, &out); err != nil {
		t.Fatal(err)
	}
	if !sd.active["authn-bridge@0.19.4.service"] || sd.active["authn-bridge@0.20.0.service"] {
		t.Errorf("after rollback: %v", sd.active)
	}
	if m, _ := readMarker(tpl); m.previous != "0.20.0" {
		t.Errorf("previous after rollback: %+v", m)
	}

	// ⛔ uninstall of the running package is refused, and touches NOTHING.
	before := tree(t, store, prefix, unitDir, sysusersDir)
	err = cmdUninstall([]string{bridge}, prefix)
	want := "refusing to uninstall: a service runs it, and nothing was removed:\n" +
		"  authn-bridge@0.19.4.service (enabled, active) runs " + filepath.Join(store, bridge, "v0.19.4") + "\n" +
		"run \"pkgm service remove authn-bridge\" first"
	if err == nil || err.Error() != want {
		t.Fatalf("uninstall of a running service:\n got %v\nwant %s", err, want)
	}
	if after := tree(t, store, prefix, unitDir, sysusersDir); after != before {
		t.Errorf("a refused uninstall changed the disk:\nbefore\n%s\nafter\n%s", before, after)
	}

	out.Reset()
	if err := cmdService([]string{"remove", "authn-bridge"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tpl); !os.IsNotExist(err) || len(enabledVersions("authn-bridge")) != 0 || sd.active["authn-bridge@0.19.4.service"] {
		t.Errorf("after remove: template err=%v enabled=%v active=%v", err, enabledVersions("authn-bridge"), sd.active)
	}
	if err := cmdUninstall([]string{bridge}, prefix); err != nil {
		t.Fatalf("uninstall after remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store, bridge)); !os.IsNotExist(err) {
		t.Errorf("the store directory survived uninstall: %v", err)
	}
	out.Reset()
	if err := serviceList(&out); err != nil || !strings.Contains(out.String(), "no pkgm services") {
		t.Errorf("list of nothing: %q %v", out.String(), err)
	}
}

// A new version that does not stay up gives the service back to the old one.
func TestSwitchRestoresTheOldVersionWhenTheNewOneFails(t *testing.T) {
	sd := withSystemd(t)
	withBridge(t)
	var out bytes.Buffer
	if err := cmdService([]string{"install", bridge + "@0.19.4"}, &out); err != nil {
		t.Fatal(err)
	}
	sd.failing["authn-bridge@0.20.0.service"] = true
	err := cmdService([]string{"switch", bridge + "@0.20.0"}, &out)
	if err == nil || !strings.Contains(err.Error(), "authn-bridge@0.20.0.service did not stay active (failed), so authn-bridge@0.19.4.service is running again") {
		t.Fatalf("switch to a failing version: %v", err)
	}
	if !sd.active["authn-bridge@0.19.4.service"] || sd.active["authn-bridge@0.20.0.service"] {
		t.Errorf("after a failed switch: %v", sd.active)
	}
	if got := enabledVersions("authn-bridge"); strings.Join(got, ",") != "0.19.4" {
		t.Errorf("enabled after a failed switch: %v", got)
	}
	if m, _ := readMarker(filepath.Join(unitDir, "authn-bridge@.service")); m.previous != "" {
		t.Errorf("a failed switch recorded a previous version: %+v", m)
	}
	// And a new version that never starts at all, likewise.
	sd.failing["authn-bridge@0.20.0.service"] = false
	oldRun := runCommand
	runCommand = func(name string, args ...string) (string, error) {
		if len(args) == 2 && args[0] == "start" && args[1] == "authn-bridge@0.20.0.service" {
			return "Job failed", errors.New("exit status 1")
		}
		return oldRun(name, args...)
	}
	if err := cmdService([]string{"switch", bridge + "@0.20.0"}, &out); err == nil || !strings.Contains(err.Error(), "is running again") {
		t.Errorf("switch to a version that cannot start: %v", err)
	}
	if !sd.active["authn-bridge@0.19.4.service"] {
		t.Errorf("the old version is not running: %v", sd.active)
	}
	// When even the old one cannot come back, say so.
	runCommand = func(name string, args ...string) (string, error) {
		if len(args) == 2 && args[0] == "start" {
			return "Job failed", errors.New("exit status 1")
		}
		return oldRun(name, args...)
	}
	if err := cmdService([]string{"switch", bridge + "@0.20.0"}, &out); err == nil || !strings.Contains(err.Error(), "could not be started again") {
		t.Errorf("switch with nothing able to start: %v", err)
	}
}

func TestInstallReportsAnInstanceThatDoesNotStayUp(t *testing.T) {
	sd := withSystemd(t)
	withBridge(t)
	sd.failing["authn-bridge@0.19.4.service"] = true
	err := cmdService([]string{"install", bridge + "@0.19.4"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "did not stay active") || !strings.Contains(err.Error(), "journalctl -u authn-bridge@0.19.4.service") {
		t.Errorf("install of an instance that dies: %v", err)
	}
}

// --- the uninstall refusal, case by case ---------------------------------------------------

// writeTemplate puts a template on disk as pkgm would, and enables or starts
// instances of it.
func writeTemplate(t *testing.T, svc, firstLine, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(unitDir, svc+"@.service"), []byte(firstLine+"\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func enable(t *testing.T, unit string) {
	t.Helper()
	d := filepath.Join(unitDir, "multi-user.target.wants")
	_ = os.MkdirAll(d, 0o755)
	if err := os.Symlink("../x@.service", filepath.Join(d, unit)); err != nil {
		t.Fatal(err)
	}
}

func installedStore(t *testing.T, projects ...string) string {
	t.Helper()
	store := t.TempDir()
	t.Setenv("PKGX_DIR", store)
	for _, p := range projects {
		if err := os.MkdirAll(filepath.Join(store, p, "v1.0.0", "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestUninstallRefusal(t *testing.T) {
	t.Run("enabled, with no systemctl at all", func(t *testing.T) {
		withSystemd(t)
		lookPath = func(string) (string, error) { return "", errors.New("not found") }
		store := installedStore(t, "acme.org/d")
		writeTemplate(t, "d", "# pkgm-service project=acme.org/d store="+store, "")
		enable(t, "d@1.0.0.service")
		err := cmdUninstall([]string{"acme.org/d"}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "d@1.0.0.service (enabled) runs") {
			t.Errorf("%v", err)
		}
	})
	t.Run("active but not enabled", func(t *testing.T) {
		sd := withSystemd(t)
		store := installedStore(t, "acme.org/d")
		writeTemplate(t, "d", "# pkgm-service project=acme.org/d store="+store, "")
		sd.active["d@1.0.0.service"] = true
		err := cmdUninstall([]string{"acme.org/d"}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "d@1.0.0.service (active) runs") {
			t.Errorf("%v", err)
		}
	})
	t.Run("systemctl fails: not read as inactive", func(t *testing.T) {
		sd := withSystemd(t)
		store := installedStore(t, "acme.org/d")
		writeTemplate(t, "d", "# pkgm-service project=acme.org/d store="+store, "")
		sd.listErr = true
		err := cmdUninstall([]string{"acme.org/d"}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "could not ask systemd whether d is running") || !strings.Contains(err.Error(), "nothing was removed") {
			t.Errorf("%v", err)
		}
		if _, err := os.Stat(filepath.Join(store, "acme.org/d")); err != nil {
			t.Errorf("the store went: %v", err)
		}
	})
	t.Run("a project that only STARTS the same way is not it", func(t *testing.T) {
		sd := withSystemd(t)
		store := installedStore(t, "gnu.org/gcc", "gnu.org/gcc/libstdcxx")
		writeTemplate(t, "gcc", "# pkgm-service project=gnu.org/gcc/libstdcxx store="+store, "")
		sd.active["gcc@1.0.0.service"] = true
		if err := cmdUninstall([]string{"gnu.org/gcc"}, t.TempDir()); err != nil {
			t.Errorf("%v", err)
		}
	})
	t.Run("only the FIRST line says whose unit it is", func(t *testing.T) {
		sd := withSystemd(t)
		store := installedStore(t, "acme.org/d", "acme.org/lib")
		// The unit of acme.org/d names acme.org/lib's directory further down,
		// as an Environment= would. Uninstalling the library must not be
		// refused on its account — and a file with no marker at all is
		// nobody's.
		writeTemplate(t, "d", "# pkgm-service project=acme.org/d store="+store,
			"Environment=LD_LIBRARY_PATH="+filepath.Join(store, "acme.org/lib/v1.0.0/lib")+"\n# pkgm-service project=acme.org/lib store="+store+"\n")
		writeTemplate(t, "other", "[Unit]", "# pkgm-service project=acme.org/lib store="+store+"\n")
		sd.active["d@1.0.0.service"] = true
		sd.active["other@1.0.0.service"] = true
		if err := cmdUninstall([]string{"acme.org/lib"}, t.TempDir()); err != nil {
			t.Errorf("%v", err)
		}
	})
	t.Run("another store is another installation", func(t *testing.T) {
		sd := withSystemd(t)
		installedStore(t, "acme.org/d")
		writeTemplate(t, "d", "# pkgm-service project=acme.org/d store=/somewhere/else", "")
		sd.active["d@1.0.0.service"] = true
		if err := cmdUninstall([]string{"acme.org/d"}, t.TempDir()); err != nil {
			t.Errorf("%v", err)
		}
	})
	t.Run("a template with nothing enabled or running", func(t *testing.T) {
		withSystemd(t)
		store := installedStore(t, "acme.org/d")
		writeTemplate(t, "d", "# pkgm-service project=acme.org/d store="+store+"/", "")
		if err := cmdUninstall([]string{"acme.org/d"}, t.TempDir()); err != nil {
			t.Errorf("%v", err)
		}
	})
	t.Run("several packages: refused as a whole", func(t *testing.T) {
		withSystemd(t)
		store := installedStore(t, "acme.org/a", "acme.org/d")
		writeTemplate(t, "d", "# pkgm-service project=acme.org/d store="+store, "")
		enable(t, "d@1.0.0.service")
		prefix := t.TempDir()
		stubFor(t, filepath.Join(prefix, "bin"), store, "acme.org/a", "1.0.0", "a")
		before := tree(t, store, prefix)
		if err := cmdUninstall([]string{"acme.org/a", "acme.org/d"}, prefix); err == nil {
			t.Fatal("not refused")
		}
		if after := tree(t, store, prefix); after != before {
			t.Errorf("the first package was removed before the second was refused:\n%s\n---\n%s", before, after)
		}
	})
	t.Run("an unreadable unit directory refuses", func(t *testing.T) {
		withSystemd(t)
		installedStore(t, "acme.org/d")
		unitDir = filepath.Join(t.TempDir(), "file")
		_ = os.WriteFile(unitDir, nil, 0o644)
		if err := cmdUninstall([]string{"acme.org/d"}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "nothing was removed") {
			t.Errorf("%v", err)
		}
	})
	t.Run("no unit directory at all", func(t *testing.T) {
		withSystemd(t)
		installedStore(t, "acme.org/d")
		unitDir = filepath.Join(t.TempDir(), "absent")
		if err := cmdUninstall([]string{"acme.org/d"}, t.TempDir()); err != nil {
			t.Errorf("%v", err)
		}
	})
}

// ⛔ ONLY `uninstall` DELETES FROM THE STORE. The refusal guards cmdUninstall
// alone because `update` and `pin` install beside what is there; if either
// ever starts removing a version, a running service would lose its binary
// with no refusal in the way. Pinned twice: by behaviour, and by reading the
// source for every call that removes a directory tree.
func TestOnlyUninstallRemovesFromTheStore(t *testing.T) {
	defer fakeServer(t, map[string]fakePkg{
		"acme.org/tool": {versions: []string{"1.0.0", "2.0.0"}, yaml: "provides:\n  - bin/tool\n", files: map[string]string{"bin/tool": "#!x\n"}},
	})()
	store := t.TempDir()
	t.Setenv("PKGX_DIR", store)
	t.Setenv("PKGM_PREFIX", t.TempDir())
	t.Setenv("PATH", "")
	if err := dispatch("pin", []string{"acme.org/tool@1.0.0"}, flags{}); err != nil {
		t.Fatal(err)
	}
	if err := dispatch("update", nil, flags{}); err != nil {
		t.Fatal(err)
	}
	if err := dispatch("pin", []string{"acme.org/tool@2.0.0"}, flags{}); err != nil {
		t.Fatal(err)
	}
	if err := dispatch("pin", []string{"acme.org/tool@1.0.0"}, flags{}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v1.0.0", "v2.0.0"} {
		if _, err := os.Stat(filepath.Join(store, "acme.org/tool", v)); err != nil {
			t.Errorf("%s was removed by update or pin: %v", v, err)
		}
	}

	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	var sites []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "os" && sel.Sel.Name == "RemoveAll" {
						sites = append(sites, fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if strings.Join(sites, ",") != "cmdUninstall" {
		t.Errorf("os.RemoveAll is called from %v; only cmdUninstall may delete from the store, and it is the one guarded", sites)
	}
}

// --- no systemd -----------------------------------------------------------------------------

func TestServiceRefusesWithoutSystemd(t *testing.T) {
	withSystemd(t)
	systemdRunDir = filepath.Join(t.TempDir(), "absent")
	lookPath = func(string) (string, error) {
		return "", errors.New("exec: \"systemctl\": executable file not found in $PATH")
	}
	for _, args := range [][]string{
		{"install", bridge},
		{"switch", bridge + "@0.20.0"},
		{"rollback", "authn-bridge"},
		{"remove", "authn-bridge"},
	} {
		err := cmdService(args, &bytes.Buffer{})
		want := "pkgm service " + args[0] + " needs systemd, and this system is not running it (" + systemdRunDir + " does not exist): " +
			"a container or FROM-scratch image has no service manager, so run the program directly, e.g. with `pkgm run`"
		if err == nil || err.Error() != want {
			t.Errorf("%v:\n got %v\nwant %s", args, err, want)
		}
	}
	// list still answers, from the disk.
	var out bytes.Buffer
	if err := cmdService([]string{"list"}, &out); err != nil || !strings.Contains(out.String(), "no pkgm services") {
		t.Errorf("list without systemd: %q %v", out.String(), err)
	}
}

func TestServicePreconditions(t *testing.T) {
	withSystemd(t)
	lookPath = func(name string) (string, error) { return "", errors.New("not found") }
	if err := cmdService([]string{"install", bridge}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "systemctl is not on PATH") {
		t.Errorf("no systemctl: %v", err)
	}
	lookPath = func(name string) (string, error) { return "/fake/" + name, nil }
	geteuid = func() int { return 1000 }
	if err := cmdService([]string{"install", bridge}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "must run as root") {
		t.Errorf("not root: %v", err)
	}
	geteuid = func() int { return 0 }
	lookPath = func(name string) (string, error) {
		if name == "systemd-sysusers" {
			return "", errors.New("not found")
		}
		return "/fake/" + name, nil
	}
	withBridge(t)
	if err := cmdService([]string{"install", bridge + "@0.19.4"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "systemd-sysusers") {
		t.Errorf("no systemd-sysusers: %v", err)
	}
}

func TestServiceInstallRefusals(t *testing.T) {
	withSystemd(t)
	withBridge(t)
	fetchSvc = func(string) (*bottle.Service, error) { return nil, nil }
	if err := cmdService([]string{"install", bridge}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "declares no service") {
		t.Errorf("no service block: %v", err)
	}
	fetchSvc = func(string) (*bottle.Service, error) { return nil, errors.New("offline") }
	if err := cmdService([]string{"install", bridge}, &bytes.Buffer{}); err == nil || err.Error() != "offline" {
		t.Errorf("fetch error: %v", err)
	}
	s := serviceFixture(t, "authn-bridge")
	fetchSvc = func(string) (*bottle.Service, error) { return s, nil }
	if err := cmdService([]string{"install", bridge + "@9.9.9"}, &bytes.Buffer{}); err == nil {
		t.Error("an unavailable version installed")
	}
	reachable = func(bin, user string) error { return errors.New("unreachable: " + user) }
	if err := cmdService([]string{"install", bridge + "@0.19.4"}, &bytes.Buffer{}); err == nil || err.Error() != "unreachable: authn-bridge" {
		t.Errorf("a program the account cannot reach: %v", err)
	}
	reachable = func(string, string) error { return nil }
	// Somebody else's unit by the same name is left alone.
	_ = os.WriteFile(filepath.Join(unitDir, "authn-bridge@.service"), []byte("[Unit]\n"), 0o644)
	if err := cmdService([]string{"install", bridge + "@0.19.4"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "was not written by pkgm") {
		t.Errorf("foreign template: %v", err)
	}
	// Another project's service by the same name, likewise.
	_ = os.WriteFile(filepath.Join(unitDir, "authn-bridge@.service"), []byte("# pkgm-service project=acme.org/x store=/opt/pkgx\n"), 0o644)
	if err := cmdService([]string{"install", bridge + "@0.19.4"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "is the service of acme.org/x") {
		t.Errorf("another project's template: %v", err)
	}
	_ = os.Remove(filepath.Join(unitDir, "authn-bridge@.service"))
	_ = os.WriteFile(filepath.Join(sysusersDir, "pkgm-authn-bridge.conf"), []byte("u x\n"), 0o644)
	if err := cmdService([]string{"install", bridge + "@0.19.4"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "was not written by pkgm") {
		t.Errorf("foreign sysusers file: %v", err)
	}
}

func TestSwitchAndRollbackRefusals(t *testing.T) {
	withSystemd(t)
	withBridge(t)
	var out bytes.Buffer
	if err := cmdService([]string{"switch", bridge}, &out); err == nil || !strings.Contains(err.Error(), "needs a version") {
		t.Errorf("switch without a version: %v", err)
	}
	if err := cmdService([]string{"switch", bridge + "@0.20.0"}, &out); err == nil || !strings.Contains(err.Error(), "not a pkgm service") {
		t.Errorf("switch of nothing: %v", err)
	}
	if err := cmdService([]string{"rollback", "authn-bridge"}, &out); err == nil || !strings.Contains(err.Error(), "not a pkgm service") {
		t.Errorf("rollback of nothing: %v", err)
	}
	if err := cmdService([]string{"install", bridge + "@0.19.4"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := cmdService([]string{"rollback", "authn-bridge"}, &out); err == nil || !strings.Contains(err.Error(), "has not been switched") {
		t.Errorf("rollback before any switch: %v", err)
	}
	tpl := filepath.Join(unitDir, "authn-bridge@.service")
	b, _ := os.ReadFile(tpl)
	_, rest, _ := strings.Cut(string(b), "\n")
	set := func(line string) { _ = os.WriteFile(tpl, []byte(line+"\n"+rest), 0o644) }
	set("# pkgm-service project=" + bridge + " store=" + bottle.Dir() + " previous=0.1.0")
	if err := cmdService([]string{"rollback", "authn-bridge"}, &out); err == nil || !strings.Contains(err.Error(), "no longer in the store") {
		t.Errorf("rollback to a version that is gone: %v", err)
	}
	set("# pkgm-service project=" + bridge + " store=" + bottle.Dir() + " previous=../../x")
	if err := cmdService([]string{"rollback", "authn-bridge"}, &out); err == nil || !strings.Contains(err.Error(), "is not a version") {
		t.Errorf("rollback to a crafted previous: %v", err)
	}
	enable(t, "authn-bridge@0.20.0.service")
	if err := cmdService([]string{"switch", bridge + "@0.20.0"}, &out); err == nil || !strings.Contains(err.Error(), "several enabled versions") {
		t.Errorf("switch with two enabled: %v", err)
	}
	for _, v := range []string{"0.19.4", "0.20.0"} {
		_ = os.Remove(filepath.Join(unitDir, "multi-user.target.wants", "authn-bridge@"+v+".service"))
	}
	if err := cmdService([]string{"switch", bridge + "@0.20.0"}, &out); err == nil || !strings.Contains(err.Error(), "no enabled version") {
		t.Errorf("switch with none enabled: %v", err)
	}
}

func TestServiceRemoveRefusals(t *testing.T) {
	sd := withSystemd(t)
	if err := cmdService([]string{"remove", "nope"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not a pkgm service") {
		t.Errorf("remove of nothing: %v", err)
	}
	writeTemplate(t, "d", "[Unit]", "")
	if err := cmdService([]string{"remove", "d"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "was not written by pkgm") {
		t.Errorf("remove of a foreign unit: %v", err)
	}
	writeTemplate(t, "d", "# pkgm-service project=acme.org/d store=/opt/pkgx", "")
	sd.listErr = true
	if err := cmdService([]string{"remove", "d"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "list-units") {
		t.Errorf("remove when systemd cannot be asked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unitDir, "d@.service")); err != nil {
		t.Errorf("the template went anyway: %v", err)
	}
}

func TestServiceListStates(t *testing.T) {
	sd := withSystemd(t)
	writeTemplate(t, "d", "# pkgm-service project=acme.org/d store=/opt/pkgx", "")
	writeTemplate(t, "e", "# pkgm-service project=acme.org/e store=/opt/pkgx", "")
	_ = os.WriteFile(filepath.Join(unitDir, "plain.service"), nil, 0o644)
	_ = os.Mkdir(filepath.Join(unitDir, "dir@.service"), 0o755)
	enable(t, "d@1.0.0.service")
	sd.active["d@2.0.0.service"] = true
	var out bytes.Buffer
	if err := serviceList(&out); err != nil {
		t.Fatal(err)
	}
	want := "d\tacme.org/d\t1.0.0 (enabled, inactive); 2.0.0 (active)\ne\tacme.org/e\tnothing enabled or running\n"
	if out.String() != want {
		t.Errorf("list:\n got %q\nwant %q", out.String(), want)
	}
	sd.listErr = true
	out.Reset()
	_ = serviceList(&out)
	if !strings.Contains(out.String(), "1.0.0 (enabled, active: unknown)") {
		t.Errorf("list when systemd cannot be asked: %q", out.String())
	}
	unitDir = filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(unitDir, nil, 0o644)
	if err := serviceList(&out); err == nil {
		t.Error("an unreadable unit directory listed as empty")
	}
}

// --- small parts ----------------------------------------------------------------------------

func TestCmdServiceArguments(t *testing.T) {
	withSystemd(t)
	for _, args := range [][]string{nil, {"bogus"}, {"install"}, {"switch", "a", "b"}, {"rollback"}, {"remove"}} {
		if err := cmdService(args, &bytes.Buffer{}); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if err := dispatch("service", []string{"list"}, flags{}); err != nil {
		t.Errorf("dispatch: %v", err)
	}
}

func TestParseMarker(t *testing.T) {
	m, ok := parseMarker("# pkgm-service project=a.org/b store=/opt/pkgx previous=1.2.3 future=x\n")
	if !ok || m != (unitMarker{project: "a.org/b", store: "/opt/pkgx", previous: "1.2.3"}) {
		t.Errorf("%+v %v", m, ok)
	}
	for _, l := range []string{"", "[Unit]", "# pkgm-service store=/x", "# pkgm-service project=a", "#pkgm-service project=a store=/x"} {
		if _, ok := parseMarker(l); ok {
			t.Errorf("%q parsed as a marker", l)
		}
	}
	if m.String() != "# pkgm-service project=a.org/b store=/opt/pkgx previous=1.2.3" {
		t.Errorf("%s", m)
	}
	if _, ok := readMarker(filepath.Join(t.TempDir(), "absent")); ok {
		t.Error("a missing file has a marker")
	}
}

func TestServiceConstraint(t *testing.T) {
	for in, want := range map[string]string{
		"a.org/b": "*", "a.org/b@0.20": "=0.20", "a.org/b@=0.20.0": "=0.20.0",
		"a.org/b@^0.20": "^0.20", "a.org/b@~1.2": "~1.2", "a.org/b@>=1": ">=1",
	} {
		if _, c := serviceConstraint(in); c != want {
			t.Errorf("%s → %s, want %s", in, c, want)
		}
	}
}

func TestCheckStore(t *testing.T) {
	for _, s := range []string{"/opt/pkgx", "/srv/pkgx", "/pkgx", "/homework"} {
		if err := checkStore(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	for s, want := range map[string]string{
		"/root/.pkgx":      "is under /root",
		"/home/me/.pkgx":   "is under /home",
		"/tmp/x":           "is under /tmp",
		"/run/user/0/pkgx": "is under /run/user",
		"relative":         "absolute path",
		"/opt/my pkgx":     "absolute path",
		"/opt/%i":          "absolute path",
	} {
		if err := checkStore(s); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", s, err)
		}
	}
}

func TestCheckReachable(t *testing.T) {
	if err := checkReachable("/", "x"); err != nil {
		t.Errorf("/: %v", err)
	}
	d := t.TempDir()
	locked := filepath.Join(d, "locked")
	_ = os.Mkdir(locked, 0o700)
	if err := checkReachable(locked, "svc"); err == nil || !strings.Contains(err.Error(), "svc, which cannot reach") {
		t.Errorf("a 0700 directory: %v", err)
	}
	if err := checkReachable(filepath.Join(d, "absent"), "svc"); err == nil {
		t.Error("a missing path is reachable")
	}
}

func TestCheckProgram(t *testing.T) {
	d := t.TempDir()
	static, dynamic, script := filepath.Join(d, "s"), filepath.Join(d, "d"), filepath.Join(d, "sh")
	_ = os.WriteFile(static, elfBytes(""), 0o755)
	_ = os.WriteFile(dynamic, elfBytes("/lib/ld-linux-aarch64.so.1"), 0o755)
	_ = os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755)
	s := &bottle.Service{Name: "x", Command: "x"}
	prov := []string{"bin/x"}
	if err := checkProgram("p", s, prov, static); err != nil {
		t.Errorf("static: %v", err)
	}
	for bin, want := range map[string]string{
		dynamic:                    "dynamically linked (interpreter /lib/ld-linux-aarch64.so.1)",
		script:                     "is not an ELF program",
		filepath.Join(d, "absent"): "is not in the installed bottle",
	} {
		if err := checkProgram("p", s, prov, bin); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", bin, err)
		}
	}
	if err := checkProgram("p", s, []string{"bin/y"}, static); err == nil || !strings.Contains(err.Error(), "does not provide") {
		t.Errorf("not provided: %v", err)
	}
}

func TestActiveVersionsReadsTheActiveColumn(t *testing.T) {
	old := runCommand
	defer func() { runCommand = old }()
	runCommand = func(string, ...string) (string, error) {
		return "● d@1.0.0.service loaded failed failed x\n" +
			"d@2.0.0.service loaded active running x\n" +
			"d@3.0.0.service loaded activating auto-restart x\n" +
			"d@4.0.0.service loaded inactive dead x\n" +
			"other@1.0.0.service loaded active running x\n" +
			"d@5.0.0.socket loaded active running x\n\n", nil
	}
	vs, err := activeVersions("systemctl", "d")
	if err != nil || strings.Join(vs, ",") != "2.0.0,3.0.0" {
		t.Errorf("%v %v", vs, err)
	}
}

func TestWriteOwned(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "u")
	if changed, err := writeOwned(p, "# pkgm-service project=a store=/x\nA\n"); !changed || err != nil {
		t.Errorf("new: %v %v", changed, err)
	}
	if changed, err := writeOwned(p, "# pkgm-service project=a store=/x\nA\n"); changed || err != nil {
		t.Errorf("same: %v %v", changed, err)
	}
	if err := os.Chmod(d, 0o500); err == nil {
		defer os.Chmod(d, 0o755)
		if _, err := writeOwned(p, "# pkgm-service project=a store=/x\nB\n"); err == nil && os.Geteuid() != 0 {
			t.Error("a write into a read-only directory succeeded")
		}
	}
	if _, err := writeOwned(filepath.Join(p, "under-a-file"), "x"); err == nil {
		t.Error("a path under a file was written")
	}
}

func TestSameDir(t *testing.T) {
	d := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	_ = os.Symlink(d, link)
	if !sameDir(d, d+"/") || !sameDir(d, link) || sameDir(d, t.TempDir()) || sameDir("/absent/a", "/absent/b") {
		t.Error("sameDir")
	}
}

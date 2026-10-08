package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pkgx/bottle"
)

// --- CLI-level fake pkgx server ---------------------------------------------
// A compact in-memory dist.pkgx.dev + pantry for exercising the CLI dispatch
// end-to-end. It points bottle.DistBase / bottle.PantryBase at itself.

type fakePkg struct {
	versions []string
	yaml     string
	files    map[string]string
}

func fakeServer(t *testing.T, pkgs map[string]fakePkg) func() {
	t.Helper()
	osn, arch := bottle.HostSlug()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if strings.HasSuffix(p, "/package.yml") {
			proj := strings.TrimSuffix(p, "/package.yml")
			if pk, ok := pkgs[proj]; ok {
				fmt.Fprint(w, pk.yaml)
				return
			}
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(p, "/versions.txt") {
			proj := strings.TrimSuffix(p, "/"+osn+"/"+arch+"/versions.txt")
			if pk, ok := pkgs[proj]; ok {
				fmt.Fprint(w, strings.Join(pk.versions, "\n"))
				return
			}
			http.NotFound(w, r)
			return
		}
		for proj, pk := range pkgs {
			pfx := proj + "/" + osn + "/" + arch + "/v"
			if !strings.HasPrefix(p, pfx) {
				continue
			}
			rest := strings.TrimPrefix(p, pfx)
			if strings.HasSuffix(rest, ".tar.gz") {
				ver := strings.TrimSuffix(rest, ".tar.gz")
				w.Write(makeBottleGz(t, proj, ver, pk.files))
				return
			}
		}
		http.NotFound(w, r)
	})
	// The fixture is a static-HTTP dist serving UNSIGNED bottles: the fail-closed
	// check (which the install path now enforces too) would refuse every one of
	// them, so this fixture states the posture it always assumed.
	t.Setenv("PKGX_VERIFY", "0")
	bottle.DistBase, bottle.PantryBase = srv.URL, srv.URL
	return srv.Close
}

func makeBottleGz(t *testing.T, project, ver string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	prefix := project + "/v" + ver + "/"
	_ = tw.WriteHeader(&tar.Header{Name: prefix, Typeflag: tar.TypeDir, Mode: 0o755})
	for rel, content := range files {
		_ = tw.WriteHeader(&tar.Header{Name: prefix + rel, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(content))})
		_, _ = tw.Write([]byte(content))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// --- flag / request parsing -------------------------------------------------

func TestParseArgs(t *testing.T) {
	pos, f := parseArgs([]string{"install", "-p", "foo", "--help"})
	if len(pos) != 2 || pos[0] != "install" || pos[1] != "foo" {
		t.Errorf("pos = %v", pos)
	}
	if !f.pin || !f.help {
		t.Errorf("flags = %+v", f)
	}
	_, f2 := parseArgs([]string{"-v"})
	if !f2.showVersion {
		t.Error("want showVersion")
	}
	// After "--", tool flags must pass through, NOT be parsed as pkgm's own.
	pos3, f3 := parseArgs([]string{"run", "nodejs.org", "--", "--version", "-h"})
	if f3.showVersion || f3.help {
		t.Errorf("flags after -- were stolen: %+v", f3)
	}
	want := []string{"run", "nodejs.org", "--", "--version", "-h"}
	if strings.Join(pos3, " ") != strings.Join(want, " ") {
		t.Errorf("passthrough pos = %v", pos3)
	}
}

func TestParseReq(t *testing.T) {
	p, c := parseReq("gnu.org/wget@1.2", false)
	if p != "gnu.org/wget" || c != "1.2" {
		t.Errorf("got %s %s", p, c)
	}
	if _, c := parseReq("gnu.org/wget@1.2", true); c != "=1.2" {
		t.Errorf("pin constraint = %s", c)
	}
	if p, c := parseReq("gnu.org/bash", false); p != "gnu.org/bash" || c != "*" {
		t.Errorf("bare = %s %s", p, c)
	}
}

func TestParsePrefixFlag(t *testing.T) {
	_, f := parseArgs([]string{"install", "--prefix", "/opt", "pkg"})
	if f.prefix != "/opt" {
		t.Errorf("--prefix value = %q", f.prefix)
	}
	_, f2 := parseArgs([]string{"install", "--prefix=/usr", "pkg"})
	if f2.prefix != "/usr" {
		t.Errorf("--prefix= value = %q", f2.prefix)
	}
	_, f3 := parseArgs([]string{"-P", "/p", "pkg"})
	if f3.prefix != "/p" {
		t.Errorf("-P value = %q", f3.prefix)
	}
}

func TestIsVersionDir(t *testing.T) {
	if !isVersionDir("v1.2.3") || isVersionDir("bin") || isVersionDir("v") {
		t.Error("isVersionDir")
	}
}

func TestDispatchUnknown(t *testing.T) {
	if err := dispatch("frob", nil, flags{}); err == nil {
		t.Fatal("want error")
	}
}

func TestResolvePrefix(t *testing.T) {
	t.Setenv("HOME", "/tmp/h")
	t.Setenv("PKGM_PREFIX", "")
	// forceLocal → ~/.local
	if p := resolvePrefix(flags{}, true); p != "/tmp/h/.local" {
		t.Errorf("forceLocal = %s", p)
	}
	// --prefix flag wins over everything
	if p := resolvePrefix(flags{prefix: "/opt/x"}, false); p != "/opt/x" {
		t.Errorf("flag prefix = %s", p)
	}
	// PKGM_PREFIX env wins when no flag
	t.Setenv("PKGM_PREFIX", "/usr")
	if p := resolvePrefix(flags{}, false); p != "/usr" {
		t.Errorf("env prefix = %s", p)
	}
	// non-root, no env/flag → ~/.local
	t.Setenv("PKGM_PREFIX", "")
	if os.Geteuid() != 0 {
		if p := resolvePrefix(flags{}, false); p != "/tmp/h/.local" {
			t.Errorf("nonroot default = %s", p)
		}
	}
}

func TestLocalPrefixNoHome(t *testing.T) {
	// A scratch container often has no usable $HOME.
	t.Setenv("HOME", "")
	if p := localPrefix(); p != "/usr/local" {
		t.Errorf("no-HOME localPrefix = %s", p)
	}
}

func TestKeysAndWarnPath(t *testing.T) {
	if k := keys(map[string]string{"b": "", "a": ""}); len(k) != 2 || k[0] != "a" {
		t.Errorf("keys = %v", k)
	}
	t.Setenv("PATH", "/usr/bin")
	warnPath("/opt") // just exercises the not-in-path branch
	t.Setenv("PATH", "/opt/bin")
	warnPath("/opt") // in-path branch
}

// --- end-to-end dispatch ----------------------------------------------------

func TestCommandsE2E(t *testing.T) {
	defer fakeServer(t, map[string]fakePkg{
		"acme.org/tool": {
			versions: []string{"1.0.0", "2.0.0"},
			yaml:     "provides:\n  - bin/tool\n",
			files:    map[string]string{"bin/tool": "#!x\n"},
		},
	})()
	home := t.TempDir()
	dir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PKGX_DIR", dir)
	t.Setenv("PATH", filepath.Join(home, ".local", "bin")) // silence warnPath
	// Name the prefix rather than inheriting resolvePrefix's default. That
	// default is euid-dependent — root installs to /usr/local, which is a
	// first-class mode here — so under a VM or container that runs as root this
	// test looked for a stub under $HOME that pkgm had correctly put elsewhere.
	t.Setenv("PKGM_PREFIX", filepath.Join(home, ".local"))

	// install pinned 1.0.0 so outdated has something to report
	if err := dispatch("install", []string{"acme.org/tool@1.0.0"}, flags{pin: true}); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(home, ".local", "bin", "tool")
	b, err := os.ReadFile(stub)
	if err != nil || !strings.HasPrefix(string(b), "#!/bin/sh") {
		t.Fatalf("stub = %q err=%v", b, err)
	}
	if err := cmdList(""); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := cmdOutdated(&out, ""); err != nil { // 1.0.0 -> 2.0.0
		t.Fatal(err)
	}
	// It is not enough that it returned nil: the point of the command is what
	// it PRINTS, and this call used to be asserted on its error alone.
	if !strings.Contains(out.String(), "acme.org/tool 1.0.0 → 2.0.0") {
		t.Errorf("the outdated package is not reported:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "1 installed, 1 checked, 1 behind, 0 could not be asked") {
		t.Errorf("the totals are wrong or missing:\n%s", out.String())
	}
	if err := cmdUpdate(resolvePrefix(flags{}, false)); err != nil {
		t.Fatal(err)
	}
	// after update the installed version is 2.0.0
	if got := installedProjects(dir); len(got) == 0 || !strings.Contains(strings.Join(got, ","), "v2.0.0") {
		t.Errorf("after update: %v", got)
	}
	// shim + uninstall
	if err := dispatch("shim", []string{"acme.org/tool"}, flags{}); err != nil {
		t.Fatal(err)
	}
	if err := dispatch("rm", []string{"acme.org/tool"}, flags{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stub); !os.IsNotExist(err) {
		t.Error("stub not removed")
	}
}

func TestInstallErrors(t *testing.T) {
	if err := cmdInstall(nil, "/tmp", flags{}); err == nil {
		t.Error("want error on empty install")
	}
	if err := cmdShim(nil, "/tmp"); err == nil {
		t.Error("want error on empty shim")
	}
	if err := cmdUninstall(nil, "/tmp"); err == nil {
		t.Error("want error on empty uninstall")
	}
	if err := cmdRun(nil); err == nil {
		t.Error("want error on empty run")
	}
}

func TestRunExec(t *testing.T) {
	// On linux, cmdRun also pulls gnu.org/glibc and exec's through the loader,
	// so the fake server must serve it too.
	defer fakeServer(t, map[string]fakePkg{
		"acme.org/tool": {
			versions: []string{"1.0.0"},
			yaml:     "provides:\n  - bin/tool\n",
			files:    map[string]string{"bin/tool": "#!x\n"},
		},
		"gnu.org/glibc": {
			versions: []string{"2.44.0"},
			yaml:     "provides:\n  - bin/ldd\n",
			files: map[string]string{
				"lib/glibc-2.44/ld-linux-x86-64.so.2":  "x",
				"lib/glibc-2.44/ld-linux-aarch64.so.1": "x",
			},
		},
	})()
	dir := t.TempDir()
	t.Setenv("PKGX_DIR", dir)
	var gotArgv []string
	old := bottle.Exec
	bottle.Exec = func(argv0 string, argv []string, env []string) error {
		gotArgv = argv
		return nil
	}
	defer func() { bottle.Exec = old }()
	if err := cmdRun([]string{"acme.org/tool", "--", "--flag"}); err != nil {
		t.Fatal(err)
	}
	// Platform-agnostic: the target binary appears in argv (directly on darwin,
	// after the loader + --library-path on linux) and trailing args survive.
	joined := strings.Join(gotArgv, " ")
	if !strings.Contains(joined, "bin/tool") {
		t.Errorf("target bin not in argv: %v", gotArgv)
	}
	if gotArgv[len(gotArgv)-1] != "--flag" {
		t.Errorf("trailing arg lost: %v", gotArgv)
	}
}

// TestReadPackageLists: a long install belongs in a committed file, so the list
// must survive comments, blank lines and inline annotations — the things that
// make such a file worth reviewing.
func TestReadPackageLists(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "toolchain.txt")
	if err := os.WriteFile(a, []byte(`# the C toolchain
llvm.org        # clang, lld, compiler-rt
gnu.org/make

  gnu.org/glibc@2.27.0   # the HPC floor

# nothing below this line
`), 0o644); err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(dir, "extra.txt")
	if err := os.WriteFile(b, []byte("curl.se\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readPackageLists([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"llvm.org", "gnu.org/make", "gnu.org/glibc@2.27.0", "curl.se"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("specs = %v, want %v", got, want)
	}

	// an empty or comment-only file contributes nothing, and is not an error
	c := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(c, []byte("# nothing here\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readPackageLists([]string{c}); err != nil || len(got) != 0 {
		t.Fatalf("empty list → %v, %v", got, err)
	}
	// no files at all
	if got, err := readPackageLists(nil); err != nil || got != nil {
		t.Fatalf("no files → %v, %v", got, err)
	}
	// a missing file is reported, not silently skipped: a committed list that
	// does not exist is a mistake worth surfacing
	if _, err := readPackageLists([]string{filepath.Join(dir, "absent.txt")}); err == nil {
		t.Fatal("want an error for a missing package list")
	}
}

// TestParseArgsFileFlag: -f/--file/--file= all collect, and repeat.
func TestParseArgsFileFlag(t *testing.T) {
	pos, f := parseArgs([]string{"install", "-f", "a.txt", "--file", "b.txt", "--file=c.txt", "extra.org"})
	if strings.Join(f.files, ",") != "a.txt,b.txt,c.txt" {
		t.Fatalf("files = %v", f.files)
	}
	if strings.Join(pos, ",") != "install,extra.org" {
		t.Fatalf("positional = %v", pos)
	}
	// a dangling -f consumes nothing rather than eating the next command
	if _, f := parseArgs([]string{"install", "-f"}); len(f.files) != 0 {
		t.Fatalf("dangling -f → %v", f.files)
	}
}

// ⛔ A LIBRARY HAS NO COMMAND, AND THE KERNEL CANNOT SAY SO. Measured
// 2026-10-08 against the real registry in a FROM-scratch image:
// `pkgm run zlib.net -- --version` downloaded the entire closure and then
// printed "pkgm: no such file or directory". zlib.net declares `provides: []`,
// so the binary name was invented from the project's leaf name and exec'd
// blind. The message names neither the package nor the reason.
func TestRunRefusesAPackageWithNoCommand(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "zlib.net")
	err := checkRunnable("zlib.net", nil, missing)
	if err == nil {
		t.Fatal("a package with no command was accepted for running")
	}
	got := err.Error()
	// THE PACKAGE IS NAMED. The defect was an error that named nothing.
	if !strings.Contains(got, "zlib.net") {
		t.Errorf("the package is not named: %q", got)
	}
	// AND THE REASON IS GIVEN, in the reader's terms: it is a library.
	if !strings.Contains(got, "library") {
		t.Errorf("the reason is not given: %q", got)
	}
	// ⛔ THE REGRESSION ITSELF: the bare errno sentence, which is what the
	// kernel supplied and what this exists to replace.
	if strings.Contains(got, "no such file or directory") {
		t.Errorf("still the kernel's own message: %q", got)
	}
}

// A DECLARED COMMAND THAT IS MISSING IS A DIFFERENT FAULT — the package is
// wrong, or our build of it is — so that one names the path somebody would
// have to go and look at.
func TestRunNamesAMissingDeclaredCommand(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "jq")
	err := checkRunnable("stedolan.github.io/jq", []string{"bin/jq"}, missing)
	if err == nil {
		t.Fatal("a missing declared command was accepted for running")
	}
	got := err.Error()
	if !strings.Contains(got, "bin/jq") || !strings.Contains(got, missing) {
		t.Errorf("neither what was declared nor where we looked: %q", got)
	}
	// It must NOT claim the package is a library: it declares a command.
	if strings.Contains(got, "library") {
		t.Errorf("a package that declares a command was called a library: %q", got)
	}
}

// AND THE POSITIVE CONTROL, without which the two refusals above would pass
// just as well on a check that refused everything.
func TestRunAcceptsABinaryThatIsThere(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "jq")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkRunnable("stedolan.github.io/jq", []string{"bin/jq"}, bin); err != nil {
		t.Errorf("a binary that exists was refused: %v", err)
	}
	// A package with no `provides:` whose guessed name HAPPENS to be right is
	// still runnable — the guess is only ever wrong when the file is absent.
	guessed := filepath.Join(t.TempDir(), "make")
	if err := os.WriteFile(guessed, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkRunnable("gnu.org/make", nil, guessed); err != nil {
		t.Errorf("a package whose guessed binary exists was refused: %v", err)
	}
}

// ⛔ AN ANSWER IT NEVER OBTAINED MUST NOT READ AS A NEGATIVE. Measured
// 2026-10-08 in a FROM-scratch container with `--network none`:
// `pkgm outdated` printed NOTHING and exited 0, with two packages installed
// and not one of them checked — every lookup had failed on DNS. The output
// was indistinguishable from "you are up to date", which is the one thing it
// had no evidence for.
func TestOutdatedDoesNotCallAFailedLookupAnAllClear(t *testing.T) {
	// A server that knows nothing: every lookup 404s, which is what an
	// unreachable pantry looks like from inside PickVersion.
	defer fakeServer(t, map[string]fakePkg{})()
	dir := t.TempDir()
	t.Setenv("PKGX_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "acme.org", "ghost", "v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := cmdOutdated(&out, "")
	// THE EXIT STATUS IS PART OF THE ANSWER: a script that reads only the
	// status would otherwise be told everything is current.
	if err == nil {
		t.Error("a run that could not check a single package returned success")
	}
	got := out.String()
	if !strings.Contains(got, "acme.org/ghost") {
		t.Errorf("the package it could not ask about is not named:\n%s", got)
	}
	if !strings.Contains(got, "all-clear") {
		t.Errorf("the output does not say this is not an all-clear:\n%s", got)
	}
	if !strings.Contains(got, "1 installed, 0 checked, 0 behind, 1 could not be asked") {
		t.Errorf("the totals are wrong or missing:\n%s", got)
	}
}

// AN EMPTY STORE SAYS SO AND QUESTIONS ITSELF. Printing nothing is read as
// "all current"; it is more often the wrong PKGX_DIR.
func TestOutdatedOfAnEmptyStoreSaysSo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PKGX_DIR", dir)
	var out strings.Builder
	if err := cmdOutdated(&out, ""); err != nil {
		t.Fatalf("an empty store was an error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "no packages installed") || !strings.Contains(got, dir) {
		t.Errorf("an empty store did not question itself:\n%s", got)
	}
	if !strings.Contains(got, "0 installed, 0 checked, 0 behind, 0 could not be asked") {
		t.Errorf("the totals are wrong or missing:\n%s", got)
	}
}

// --- uninstall --------------------------------------------------------------

// stubFor writes the kind of file StubBins writes: a shell stub whose exec
// line names the binary inside the package's store directory. That exec line
// is the only durable record of what pkgm linked and for whom.
func stubFor(t *testing.T, binDir, storeDir, project, version, name string, siblings ...string) string {
	t.Helper()
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(storeDir, project, "v"+version, "bin", name)
	p := filepath.Join(binDir, name)
	// ⛔ LD_LIBRARY_PATH NAMES THE WHOLE CLOSURE, not just this package. The
	// first version of this helper wrote LD_LIBRARY_PATH="/x" — a stub that
	// diverged from a real one at precisely the point under test — and the
	// test passed while `pkgm uninstall jq` removed oniguruma's program in a
	// container. siblings are the other store paths a real stub would carry.
	libs := []string{filepath.Join(storeDir, project, "v"+version, "lib")}
	libs = append(libs, siblings...)
	body := "#!/bin/sh\nexport LD_LIBRARY_PATH=\"" + strings.Join(libs, ":") + "${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}\"\nexec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// ⛔ THE SEPARATOR IS THE RULE, as in every prefix match. Without the version
// segment in the marker, uninstalling gnu.org/gcc also unlinks
// gnu.org/gcc/libstdcxx — a different package whose store path merely starts
// the same way, and one that half the C++ bottles depend on.
func TestUninstallDoesNotTakeASubprojectWithIt(t *testing.T) {
	store, prefix := t.TempDir(), t.TempDir()
	binDir := filepath.Join(prefix, "bin")
	// Each carries the OTHER's library directory, because that is what a real
	// closure stub does — and it is what made a substring search over the
	// whole file remove a sibling's program.
	gccLib := filepath.Join(store, "gnu.org/gcc", "v14.2.0", "lib")
	cxxLib := filepath.Join(store, "gnu.org/gcc/libstdcxx", "v14.2.0", "lib")
	gcc := stubFor(t, binDir, store, "gnu.org/gcc", "14.2.0", "gcc", cxxLib)
	libstdcxx := stubFor(t, binDir, store, "gnu.org/gcc/libstdcxx", "14.2.0", "c++filt", gccLib)
	// Somebody else's program, same name as nothing of ours — and a real
	// binary, not a stub, so it also proves a non-stub file is left alone.
	theirs := filepath.Join(binDir, "gcc-wrapper")
	if err := os.WriteFile(theirs, []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0}, 0o755); err != nil {
		t.Fatal(err)
	}

	removed, err := removeStubsInto(binDir, store, "gnu.org/gcc")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != gcc {
		t.Errorf("removed %v, want just %s", removed, gcc)
	}
	if _, err := os.Stat(libstdcxx); err != nil {
		t.Errorf("uninstalling gnu.org/gcc took gnu.org/gcc/libstdcxx with it: %v", err)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("a file that is not one of our stubs was removed: %v", err)
	}
}

// ⛔ UNINSTALL MUST NOT NEED THE NETWORK. Measured 2026-10-08 in a
// FROM-scratch container with `--network none`: it failed on a DNS lookup,
// because it asked the PANTRY which binaries to delete. The pantry is also
// the wrong oracle — it answers for the recipe as it stands today, not for
// what we actually linked, so a `provides:` that changed since the install
// left stale stubs behind.
func TestUninstallWorksWithNoNetworkAtAll(t *testing.T) {
	store, prefix := t.TempDir(), t.TempDir()
	// A port nothing listens on: any fetch fails at once, loudly.
	oldDist, oldPantry := bottle.DistBase, bottle.PantryBase
	bottle.DistBase, bottle.PantryBase = "http://127.0.0.1:1", "http://127.0.0.1:1"
	defer func() { bottle.DistBase, bottle.PantryBase = oldDist, oldPantry }()
	t.Setenv("PKGX_DIR", store)

	stub := stubFor(t, filepath.Join(prefix, "bin"), store, "acme.org/tool", "1.0.0", "tool")
	if err := os.MkdirAll(filepath.Join(store, "acme.org/tool", "v1.0.0", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := cmdUninstall([]string{"acme.org/tool"}, prefix); err != nil {
		t.Fatalf("uninstall needed the network: %v", err)
	}
	if _, err := os.Stat(stub); !os.IsNotExist(err) {
		t.Errorf("the stub survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store, "acme.org/tool")); !os.IsNotExist(err) {
		t.Errorf("the store directory survived: %v", err)
	}
}

// A REMOVAL THAT REMOVED NOTHING SAYS SO. Printing nothing is how this
// command reported both success and "that was never installed".
func TestUninstallSaysWhatItDid(t *testing.T) {
	store, prefix := t.TempDir(), t.TempDir()
	t.Setenv("PKGX_DIR", store)
	out := captureStdout(t, func() {
		if err := cmdUninstall([]string{"acme.org/ghost"}, prefix); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "not installed") || !strings.Contains(out, "acme.org/ghost") {
		t.Errorf("a package that was never installed got no answer:\n%s", out)
	}

	stubFor(t, filepath.Join(prefix, "bin"), store, "acme.org/tool", "1.0.0", "tool")
	if err := os.MkdirAll(filepath.Join(store, "acme.org/tool", "v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() {
		if err := cmdUninstall([]string{"acme.org/tool"}, prefix); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "removed") || !strings.Contains(out, "1 stub(s)") {
		t.Errorf("a real removal was not reported:\n%s", out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

// ⛔⛔ THE FIXTURE THAT CANNOT DIVERGE: these stubs are written by the REAL
// bottle.StubBins, through the fake pantry, rather than by hand.
//
// Measured 2026-10-08 in a FROM-scratch container:
// `pkgm uninstall stedolan.github.io/jq` also removed /opt/bin/onig-config —
// oniguruma's program. A stub exports LD_LIBRARY_PATH holding EVERY library
// directory in the closure, so a substring search over the whole file matches
// every sibling of the same install. The hand-written fixture wrote
// LD_LIBRARY_PATH="/x" and saw none of it.
func TestUninstallLeavesTheClosureSiblingsAlone(t *testing.T) {
	defer fakeServer(t, map[string]fakePkg{
		"acme.org/tool": {
			versions: []string{"1.0.0"},
			yaml:     "provides:\n  - bin/tool\n",
			files:    map[string]string{"bin/tool": "#!x\n", "lib/libtool.so": "x"},
		},
		"acme.org/other": {
			versions: []string{"1.0.0"},
			yaml:     "provides:\n  - bin/other\n",
			files:    map[string]string{"bin/other": "#!x\n", "lib/libother.so": "x"},
		},
	})()
	home, store := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PKGX_DIR", store)
	prefix := filepath.Join(home, ".local")
	t.Setenv("PKGM_PREFIX", prefix)
	t.Setenv("PATH", filepath.Join(prefix, "bin"))

	// ONE install, so both stubs share the closure's LD_LIBRARY_PATH — which
	// is the whole hazard.
	if err := dispatch("install", []string{"acme.org/tool", "acme.org/other"}, flags{}); err != nil {
		t.Fatal(err)
	}
	toolStub := filepath.Join(prefix, "bin", "tool")
	otherStub := filepath.Join(prefix, "bin", "other")
	b, err := os.ReadFile(otherStub)
	if err != nil {
		t.Fatal(err)
	}
	// THE POSITIVE CONTROL ON THE FIXTURE ITSELF. If the sibling's stub does
	// not mention acme.org/tool, this test cannot witness the defect it
	// exists for, and its passing would mean nothing.
	if !strings.Contains(string(b), filepath.Join(store, "acme.org/tool")) {
		t.Fatalf("the fixture does not reproduce the hazard — acme.org/other's stub never names acme.org/tool:\n%s", b)
	}

	if err := cmdUninstall([]string{"acme.org/tool"}, prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(toolStub); !os.IsNotExist(err) {
		t.Errorf("the uninstalled package's stub survived: %v", err)
	}
	if _, err := os.Stat(otherStub); err != nil {
		t.Errorf("a sibling in the same closure lost its stub: %v", err)
	}
}

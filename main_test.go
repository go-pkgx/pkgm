package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
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
	if err := cmdOutdated(""); err != nil { // 1.0.0 -> 2.0.0
		t.Fatal(err)
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

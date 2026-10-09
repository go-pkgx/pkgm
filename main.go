// Command pkgm is a dependency-free, pure-Go installer for pkgx bottles.
//
// It resolves a package's runtime dependency closure from the pkgx pantry,
// downloads the bottles — from the signed OCI registry oci://ghcr.io/go-pkgx/packages
// by default, verifying each bottle's signature (fail-closed) — and installs
// them, with no runtime dependencies of its own (a single CGO_ENABLED=0 binary
// that runs on a `FROM scratch` image). Point PKGX_DIST at the unsigned upstream
// (https://dist.pkgx.dev) with PKGX_VERIFY=0 for the full pantry. It mirrors the
// reference pkgm CLI so it is a drop-in replacement, and adds a shell-free `run`
// for scratch images.
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-pkgx/bottle"
)

// osReadFile is os.ReadFile, a seam so the package-list reader's error branch is
// testable without contriving an unreadable file.
var osReadFile = os.ReadFile

// version is reported by `pkgm --version`. It defaults to "dev" and is
// overridden at release-build time via -ldflags "-X main.version=<tag>".
var version = "dev"

var usage = `pkgm ` + version + ` — pure-Go pkgx package manager

usage:
  pkgm install|i    <pkg>[@version] ...   install to /usr/local (root) or ~/.local
                    -f <file>             ... or read the list from a file
  pkgm uninstall|rm <pkg> ...             remove an installation (works offline)
  pkgm shim|stub    <pkg> ...             create a shim in <prefix>/bin
  pkgm list|ls                            list what's installed
  pkgm outdated                           what has a newer version (exit 1 if any could not be checked)
  pkgm update|up|upgrade                  update installations to latest
  pkgm pin          <pkg>@version ...     install pinned to an exact version
  pkgm run|x        <pkg> [-- args...]    run a pkg (shell-free; works FROM scratch)
  pkgm image        <pkg>                 emit a FROM-scratch Containerfile that
                                          installs <pkg> with pkgm, in the image
  pkgm service install  <pkg>[@version]  run <pkg> as a systemd service, that version
  pkgm service switch   <pkg>@version   move the service to another version
  pkgm service rollback <svc>            return to the version before the last switch
  pkgm service list                      which version each service runs
  pkgm service remove   <svc>            stop and disable it, delete its unit

flags:
  -h, --help        show this help
  -v, --version     show version
  -p, --pin         pin the requested version(s) exactly
  -P, --prefix DIR  install prefix (bins go to DIR/bin); overrides root detection
  -s, --from-scratch  also pull the implicit libc/gcc closure (FROM-scratch ready)
  -f, --file FILE   read packages from FILE, one <pkg>[@version] per line
                    (repeatable; blank lines ignored, # starts a comment). A long
                    install belongs in a file you can commit next to what it
                    builds, review and diff — not retyped on a command line.

env:
  PKGX_DIR          bottle store (default: ~/.pkgx)
  PKGX_DIST         bottle source (default: oci://ghcr.io/go-pkgx/packages, the
                    signed registry; set https://dist.pkgx.dev for the full
                    unsigned upstream pantry — pair with PKGX_VERIFY=0)
  PKGX_VERIFY       verify bottle signatures, fail-closed (default: on;
                    set 0/false/no/off to disable)
  PKGM_PREFIX       default install prefix (ideal for FROM scratch: be root,
                    set PKGM_PREFIX=/usr, no sudo needed)

  ~/.pkgx/config.hcl2 sets defaults for the PKGX_* / OCI_* variables above
  (HCL2 attributes, e.g. PKGX_DIST = "oci://ghcr.io/go-pkgx/packages"). A real
  environment variable always overrides a value set in the file.
`

type flags struct {
	help, showVersion, pin, scratch bool
	prefix                          string
	files                           []string // -f/--file: package lists to read
}

// parseArgs splits flags from positional arguments (getopt-style, matching the
// reference pkgm's -h/-v/-p aliases) and captures the value-taking
// --prefix/-P <dir> (also accepts --prefix=<dir>).
func parseArgs(argv []string) ([]string, flags) {
	var f flags
	var pos []string
	raw := false // everything after the first "--" is passed through verbatim
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if raw {
			pos = append(pos, a)
			continue
		}
		switch {
		case a == "--":
			raw = true
			pos = append(pos, a) // keep the separator so `run` sees the boundary
		case a == "-h" || a == "--help":
			f.help = true
		case a == "-v" || a == "--version":
			f.showVersion = true
		case a == "-p" || a == "--pin":
			f.pin = true
		case a == "-s" || a == "--from-scratch":
			f.scratch = true
		case a == "-P" || a == "--prefix":
			if i+1 < len(argv) {
				i++
				f.prefix = argv[i]
			}
		case strings.HasPrefix(a, "--prefix="):
			f.prefix = strings.TrimPrefix(a, "--prefix=")
		case a == "-f" || a == "--file":
			if i+1 < len(argv) {
				i++
				f.files = append(f.files, argv[i])
			}
		case strings.HasPrefix(a, "--file="):
			f.files = append(f.files, strings.TrimPrefix(a, "--file="))
		default:
			pos = append(pos, a)
		}
	}
	return pos, f
}

func main() { os.Exit(run(os.Args[1:])) }

// configError reports a load/parse failure of ~/.pkgx/config.hcl2, if any. It
// is a var so tests can drive the startup-warning path.
var configError = bottle.ConfigError

// run is the testable entry point; it returns the process exit code.
func run(argv []string) int {
	// A closure the resolver could not complete is not an error here — it is an
	// error MUCH later, when the installed program starts and reports "cannot
	// open shared object file". Print those diagnostics; bottle stays silent by
	// default. It matters most for `pkgm image`, whose FROM-scratch output has
	// no host library to paper over the gap.
	bottle.Warn = func(msg string) { fmt.Fprintln(os.Stderr, "pkgm: "+msg) }
	if err := configError(); err != nil {
		fmt.Fprintln(os.Stderr, "pkgm: warning: ignoring ~/.pkgx/config.hcl2: "+err.Error())
	}
	pos, f := parseArgs(argv)
	if f.help || (len(pos) > 0 && pos[0] == "help") {
		fmt.Print(usage)
		return 0
	}
	if f.showVersion {
		fmt.Println("pkgm " + version)
		return 0
	}
	if len(pos) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	if err := dispatch(pos[0], pos[1:], f); err != nil {
		fmt.Fprintln(os.Stderr, "pkgm: "+err.Error())
		return 1
	}
	return 0
}

func dispatch(cmd string, args []string, f flags) error {
	switch cmd {
	case "install", "i":
		return cmdInstall(args, resolvePrefix(f, false), f)
	case "local-install", "li":
		return cmdInstall(args, resolvePrefix(f, true), f)
	case "shim", "stub":
		return cmdShim(args, resolvePrefix(f, false))
	case "uninstall", "rm":
		return cmdUninstall(args, resolvePrefix(f, false))
	case "list", "ls":
		return cmdList(resolvePrefix(f, false))
	case "outdated":
		return cmdOutdated(os.Stdout, resolvePrefix(f, false))
	case "up", "update", "upgrade":
		return cmdUpdate(resolvePrefix(f, false))
	case "pin":
		f.pin = true
		return cmdInstall(args, resolvePrefix(f, false), f)
	case "run", "x":
		return cmdRun(args)
	case "image":
		return cmdImage(args)
	case "service":
		return cmdService(args, os.Stdout)
	default:
		return fmt.Errorf("unknown command %q (try --help)", cmd)
	}
}

// resolvePrefix decides where binaries are installed. Explicit wins:
// --prefix flag, then $PKGM_PREFIX (both ideal for a `FROM scratch` image
// where you are root, there is no sudo, and $HOME is unset). Otherwise it
// mirrors the reference pkgm: /usr/local as root, else ~/.local. Being root is
// a fully-supported first-class mode here — pkgm never nags to use sudo.
func resolvePrefix(f flags, forceLocal bool) string {
	if f.prefix != "" {
		return f.prefix
	}
	if p := os.Getenv("PKGM_PREFIX"); p != "" {
		return p
	}
	if forceLocal {
		return localPrefix()
	}
	if os.Geteuid() == 0 {
		return "/usr/local"
	}
	return localPrefix()
}

// localPrefix is ~/.local, falling back to the system prefix when there is no
// usable $HOME (e.g. a bare scratch container running as root).
func localPrefix() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local")
	}
	return "/usr/local"
}

// parseReq splits "project@constraint" into its parts; pin forces an exact "=".
func parseReq(s string, pin bool) (project, constraint string) {
	if i := strings.LastIndex(s, "@"); i > 0 {
		c := s[i+1:]
		if pin {
			c = "=" + strings.TrimPrefix(c, "=")
		}
		return s[:i], c
	}
	return s, "*"
}

func cmdInstall(args []string, prefix string, f flags) error {
	fromFiles, err := readPackageLists(f.files)
	if err != nil {
		return err
	}
	args = append(fromFiles, args...)
	if len(args) == 0 {
		return fmt.Errorf("no packages specified")
	}
	roots := map[string]string{}
	for _, a := range args {
		p, c := parseReq(a, f.pin)
		roots[p] = c
	}
	dir := bottle.Dir()
	var closure []bottle.Resolved
	if f.scratch {
		// Materialize the complete FROM-scratch closure (declared deps + the
		// implicit glibc/libgcc_s/libstdc++/libatomic system libraries).
		closure, err = bottle.CompleteClosure(roots, dir)
		if err != nil {
			return err
		}
		for _, r := range closure {
			fmt.Printf("  closure   %s v%s\n", r.Project, r.Version.Raw)
		}
	} else {
		closure, err = bottle.ResolveClosure(roots)
		if err != nil {
			return err
		}
		for _, r := range closure {
			fresh, err := bottle.Install(r, dir)
			if err != nil {
				return fmt.Errorf("%s: %w", r.Project, err)
			}
			state := "cached"
			if fresh {
				state = "installed"
			}
			fmt.Printf("  %-9s %s v%s\n", state, r.Project, r.Version.Raw)
		}
	}
	if f.scratch {
		// Make the rootfs itself runnable, not just populated: pose the pkgx
		// loader at the canonical PT_INTERP path and, when the closure carries a
		// shell, /bin/sh. A FROM-scratch image has neither, and anything that
		// shells out dies without them — `make` runs every recipe line through
		// /bin/sh, so a build inside such an image fails on the first one with
		// "make: /bin/sh: No such file or directory". `pkgm run` already does
		// this at run time; an image built with -s needs it baked in.
		if bottle.GOOS() == "linux" {
			if loader := bottle.FindLoader(dir); loader != "" {
				bottle.SetupScratchRootfs(loader, bottle.FindClosureBin(closure, dir, "gnu.org/bash", "bash"))
			}
		}
	}
	n, err := bottle.StubBins(closure, dir, prefix)
	if err != nil {
		return err
	}
	fmt.Printf("linked %d binaries → %s\n", n, filepath.Join(prefix, "bin"))
	warnPath(prefix)
	return nil
}

func cmdShim(args []string, prefix string) error {
	if len(args) == 0 {
		return fmt.Errorf("no packages specified")
	}
	roots := map[string]string{}
	for _, a := range args {
		p, c := parseReq(a, false)
		roots[p] = c
	}
	dir := bottle.Dir()
	closure, err := bottle.ResolveClosure(roots)
	if err != nil {
		return err
	}
	for _, r := range closure {
		if _, err := bottle.Install(r, dir); err != nil {
			return fmt.Errorf("%s: %w", r.Project, err)
		}
	}
	n, err := bottle.StubBins(closure, dir, prefix)
	if err != nil {
		return err
	}
	fmt.Printf("shimmed %d binaries → %s\n", n, filepath.Join(prefix, "bin"))
	return nil
}

// cmdUninstall removes a package's stubs and its store directory.
//
// ⛔ IT USED TO ASK THE PANTRY WHAT TO DELETE, WHICH IS THE WRONG ORACLE AND
// NEEDS THE NETWORK. Measured 2026-10-08 in a FROM-scratch container with
// `--network none`: `pkgm uninstall gnu.org/bash` failed outright on a DNS
// lookup. You could not uninstall offline — and on a host whose pantry has
// moved on, the names it fetched were the names the recipe provides TODAY,
// not the ones we actually linked, so a `provides:` that changed since the
// install left stale stubs behind and removed nothing.
//
// What we linked is written down on the machine: every stub ends in
//
//	exec "<store>/<project>/v<version>/bin/<name>" "$@"
//
// so the stubs that belong to a package are the ones that point into its
// store directory. That is exact, it is offline, and it cannot delete a
// same-named binary somebody else put in the prefix.
func cmdUninstall(args []string, prefix string) error {
	if len(args) == 0 {
		return fmt.Errorf("no packages specified")
	}
	dir := bottle.Dir()
	projects := make([]string, 0, len(args))
	for _, a := range args {
		project, _ := parseReq(a, false)
		projects = append(projects, project)
	}
	// BEFORE ANYTHING IS DELETED, stubs included, and for every argument at
	// once: a refused uninstall is a complete no-op, not one that removed the
	// first two packages and then stopped at the third.
	if err := refuseUninstallInUse(projects, dir); err != nil {
		return err
	}
	for _, a := range args {
		project, _ := parseReq(a, false)
		removed, err := removeStubsInto(filepath.Join(prefix, "bin"), dir, project)
		if err != nil {
			return fmt.Errorf("%s: %w", project, err)
		}
		for _, link := range removed {
			fmt.Printf("  removed %s\n", link)
		}
		store := filepath.Join(dir, project)
		_, statErr := os.Stat(store)
		if err := os.RemoveAll(store); err != nil {
			return fmt.Errorf("%s: %w", project, err)
		}
		// SAY WHAT HAPPENED, including when it was nothing: a command that
		// removes files and prints nothing cannot be told apart from one that
		// found nothing to remove.
		switch {
		case statErr == nil:
			fmt.Printf("%s: removed %s and %d stub(s)\n", project, store, len(removed))
		case len(removed) > 0:
			fmt.Printf("%s: not in %s; removed %d orphaned stub(s)\n", project, dir, len(removed))
		default:
			fmt.Printf("%s: not installed in %s — nothing to remove\n", project, dir)
		}
	}
	return nil
}

// removeStubsInto deletes the stubs in binDir that exec a binary out of
// project's store directory, and returns the paths it removed.
//
// ⛔ THE SEPARATOR IS THE RULE, as it is in every prefix match. The marker is
// "<store>/<project>/v", not "<store>/<project>": without the version segment,
// uninstalling gnu.org/gcc would also unlink gnu.org/gcc/libstdcxx, a
// different package whose path merely starts the same way.
//
// A file that is not one of our stubs is left alone in silence — the prefix's
// bin directory is full of other people's programs.
func removeStubsInto(binDir, storeDir, project string) ([]string, error) {
	marker := filepath.Join(storeDir, project) + string(filepath.Separator) + "v"
	entries, err := os.ReadDir(binDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(binDir, e.Name())
		// A stub is a few hundred bytes of shell. Reading one is cheap; the
		// cap keeps a real binary that landed here from being slurped whole.
		b, err := readHead(p, 8192)
		if err != nil || !strings.HasPrefix(stubTarget(b), marker) {
			continue
		}
		if err := os.Remove(p); err != nil {
			return removed, err
		}
		removed = append(removed, p)
	}
	return removed, nil
}

// stubTarget returns the binary a stub execs, or "" if the bytes are not one
// of our stubs.
//
// ⛔ ONLY THE EXEC LINE SAYS WHOSE STUB THIS IS. A stub also exports
// LD_LIBRARY_PATH holding EVERY library directory in the closure, so a
// substring search over the whole file matches every sibling in the same
// install. Measured 2026-10-08 in a FROM-scratch container:
// `pkgm uninstall stedolan.github.io/jq` removed /opt/bin/onig-config too —
// oniguruma's program, matched on jq's lib path inside its environment line.
//
// The unit test did not see it: its fixture wrote LD_LIBRARY_PATH="/x",
// a hand-written stub that diverged from a real one at precisely the point
// under test.
func stubTarget(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, `exec "`)
		if !ok {
			continue
		}
		if i := strings.Index(rest, `"`); i >= 0 {
			return rest[:i]
		}
	}
	return ""
}

func readHead(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, max)
	n, err := f.Read(b)
	if err != nil && n == 0 {
		return nil, err
	}
	return b[:n], nil
}

func cmdList(prefix string) error {
	for _, p := range installedProjects(bottle.Dir()) {
		fmt.Println(p)
	}
	return nil
}

// cmdOutdated says which installed packages have a newer version — and, with
// equal prominence, how many it could not ask about.
//
// ⛔ AN ANSWER IT NEVER OBTAINED READS AS A NEGATIVE. Measured 2026-10-08 in a
// FROM-scratch container with `--network none`: this printed NOTHING and
// exited 0, with two packages installed and not one of them checked. Every
// lookup had failed on DNS. The output was indistinguishable from "you are up
// to date", which is the one thing it had no evidence for.
//
// The silence had three sources and they all looked the same: nothing
// installed, a store entry that did not parse, and a lookup that failed. Each
// is now said out loud, and a run that could not ask about something exits
// non-zero — because "I don't know" is not "nothing to do".
func cmdOutdated(w io.Writer, prefix string) error {
	dir := bottle.Dir()
	lines := installedProjects(dir)
	var checked, behind int
	var unchecked []string
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			unchecked = append(unchecked, fmt.Sprintf("unreadable store entry %q", line))
			continue
		}
		project, have := fields[0], strings.TrimPrefix(fields[1], "v")
		latest, err := bottle.PickVersion(project, "*")
		if err != nil {
			unchecked = append(unchecked, fmt.Sprintf("%s: %v", project, err))
			continue
		}
		checked++
		if latest.Raw != have {
			behind++
			fmt.Fprintf(w, "%s %s → %s\n", project, have, latest.Raw)
		}
	}
	if len(unchecked) > 0 {
		fmt.Fprintf(w, "\ncould NOT be asked about — these are not an all-clear:\n")
		for _, u := range unchecked {
			fmt.Fprintf(w, "  %s\n", u)
		}
	}
	// THE TOTALS ALWAYS, including the zeros: a command that prints nothing
	// when all is well cannot be told apart from one that failed to look.
	fmt.Fprintf(w, "\n%d installed, %d checked, %d behind, %d could not be asked\n",
		len(lines), checked, behind, len(unchecked))
	if len(lines) == 0 {
		fmt.Fprintf(w, "no packages installed in %s — is that the right PKGX_DIR?\n", dir)
	}
	if len(unchecked) > 0 {
		return fmt.Errorf("%d of %d installed packages could not be checked", len(unchecked), len(lines))
	}
	return nil
}

func cmdUpdate(prefix string) error {
	dir := bottle.Dir()
	roots := map[string]string{}
	for _, line := range installedProjects(dir) {
		if fields := strings.Fields(line); len(fields) == 2 {
			roots[fields[0]] = "*"
		}
	}
	if len(roots) == 0 {
		return nil
	}
	return cmdInstall(keys(roots), prefix, flags{})
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// installedProjects lists "<project> v<version>" for every installed bottle.
func installedProjects(dir string) []string {
	var found []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || !info.IsDir() {
			return nil
		}
		if isVersionDir(filepath.Base(p)) {
			rel, _ := filepath.Rel(dir, filepath.Dir(p))
			if rel != "." && rel != "bin" {
				found = append(found, rel+" "+filepath.Base(p))
			}
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(found)
	return found
}

func isVersionDir(base string) bool {
	return len(base) > 1 && base[0] == 'v' && base[1] >= '0' && base[1] <= '9'
}

func warnPath(prefix string) {
	bin := filepath.Join(prefix, "bin")
	for _, p := range strings.Split(os.Getenv("PATH"), ":") {
		if p == bin {
			return
		}
	}
	fmt.Fprintf(os.Stderr, "! warning: %s is not in $PATH\n", bin)
}

// readPackageLists reads package specs from each -f/--file, one per line.
//
// A long install belongs in a FILE that lives beside the thing it builds: the
// list is then reviewable, diffable and committed with the rest of the project,
// instead of being retyped on a command line (or buried in a Dockerfile) where
// nothing records why a package is there. Blank lines are ignored and `#`
// starts a comment — so each entry can say what it is for, which is half the
// point of committing the list.
//
// Specs are exactly what the command line takes (project[@constraint]), and
// files are read BEFORE the positional arguments, so an argument can still
// override or extend a committed list.
func readPackageLists(files []string) ([]string, error) {
	var specs []string
	for _, f := range files {
		data, err := osReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("package list: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if i := strings.IndexByte(line, '#'); i >= 0 {
				line = line[:i]
			}
			if s := strings.TrimSpace(line); s != "" {
				specs = append(specs, s)
			}
		}
	}
	return specs, nil
}

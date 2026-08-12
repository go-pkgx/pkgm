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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-pkgx/bottle"
)

// version is reported by `pkgm --version`. It defaults to "dev" and is
// overridden at release-build time via -ldflags "-X main.version=<tag>".
var version = "dev"

var usage = `pkgm ` + version + ` — pure-Go pkgx package manager

usage:
  pkgm install|i    <pkg>[@version] ...   install to /usr/local (root) or ~/.local
  pkgm uninstall|rm <pkg> ...             remove an installation
  pkgm shim|stub    <pkg> ...             create a shim in <prefix>/bin
  pkgm list|ls                            list what's installed
  pkgm outdated                           list outdated installations
  pkgm update|up|upgrade                  update installations to latest
  pkgm pin          <pkg>@version ...     install pinned to an exact version
  pkgm run|x        <pkg> [-- args...]    run a pkg (shell-free; works FROM scratch)
  pkgm image        <pkg>                 emit a FROM-scratch Dockerfile that
                                          installs <pkg> with pkgm, in the image

flags:
  -h, --help        show this help
  -v, --version     show version
  -p, --pin         pin the requested version(s) exactly
  -P, --prefix DIR  install prefix (bins go to DIR/bin); overrides root detection
  -s, --from-scratch  also pull the implicit libc/gcc closure (FROM-scratch ready)

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
		return cmdOutdated(resolvePrefix(f, false))
	case "up", "update", "upgrade":
		return cmdUpdate(resolvePrefix(f, false))
	case "pin":
		f.pin = true
		return cmdInstall(args, resolvePrefix(f, false), f)
	case "run", "x":
		return cmdRun(args)
	case "image":
		return cmdImage(args)
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
	var err error
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

func cmdUninstall(args []string, prefix string) error {
	if len(args) == 0 {
		return fmt.Errorf("no packages specified")
	}
	dir := bottle.Dir()
	for _, a := range args {
		project, _ := parseReq(a, false)
		_, provides, err := bottle.FetchMeta(project)
		if err != nil {
			return err
		}
		for _, name := range bottle.BinNames(project, provides) {
			link := filepath.Join(prefix, "bin", name)
			if err := os.Remove(link); err == nil {
				fmt.Printf("  removed %s\n", link)
			}
		}
		_ = os.RemoveAll(filepath.Join(dir, project))
	}
	return nil
}

func cmdList(prefix string) error {
	for _, p := range installedProjects(bottle.Dir()) {
		fmt.Println(p)
	}
	return nil
}

func cmdOutdated(prefix string) error {
	dir := bottle.Dir()
	for _, line := range installedProjects(dir) {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		project, have := fields[0], strings.TrimPrefix(fields[1], "v")
		latest, err := bottle.PickVersion(project, "*")
		if err != nil {
			continue
		}
		if latest.Raw != have {
			fmt.Printf("%s %s → %s\n", project, have, latest.Raw)
		}
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

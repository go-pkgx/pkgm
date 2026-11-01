package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-pkgx/bottle"
)

// ⛔ A SHIM IS A PRIVILEGE BOUNDARY, AND NOTHING HERE TREATED IT AS ONE.
//
// Measured on this machine, 2026-10-31 (go-pkgx/pkgm#73). `sudo pkgm install
// go.dev@1.27.3` wrote /usr/local/bin/go — root-owned, on root's PATH —
// containing:
//
//	export LD_LIBRARY_PATH="/Users/<user>/.pkgx/go.dev/v1.27.3/lib:…"
//	exec "/Users/<user>/.pkgx/go.dev/v1.27.3/bin/go" "$@"
//
// PKGX_DIR defaults to ~/.pkgx and macOS sudo keeps the caller's HOME, so
// root's store resolved to an unprivileged user's home directory. Anything
// root ran through `go` after that executed a user-writable binary and loaded
// libraries from user-writable directories. /usr/local/pkgs was left empty.
//
// The reference pkgm does not have this hole, and not by checking for it: it
// runs the download as the pre-sudo user (`sudo -u $SUDO_USER pkgx`) and then
// MIRRORS the result into <prefix>/pkgs, so the shim's target is inside the
// prefix by construction (pkgm.ts, `mirror_directory(join(dst, "pkgs"), …)`,
// read 2026-11-01). We shim straight at the store instead, which is cheaper
// and is why the store's location had to become the prefix's business.
//
// Two changes, because either alone leaves a hole:
//
//   - storeFor makes the default store follow the prefix, so the mistake is
//     hard to make;
//   - trustShimTargets refuses it whatever the environment, so the mistake is
//     impossible to make. PKGM_PREFIX, --prefix and PKGX_DIR can be combined
//     into configurations nobody enumerated, and a default is not a boundary.

// storeFor is the bottle store an install into this prefix should use.
//
// PKGX_DIR wins: it is explicit, it is what the error message below suggests,
// and a `FROM scratch` image sets it deliberately. Otherwise the store lives
// beside the binaries it backs — <prefix>/pkgs — whenever the prefix is not
// inside the home directory of the user we are running as.
//
// That last condition, rather than "euid == 0": under `sudo` the dangerous
// thing is not being root, it is that HOME still belongs to somebody else. A
// prefix outside HOME means the two can disagree, and ~/.pkgx is then the
// wrong answer no matter what euid says. A plain `pkgm install` into ~/.local
// keeps using ~/.pkgx, so nothing changes for the ordinary case.
func storeFor(prefix string) string {
	if bottle.Env("PKGX_DIR") != "" {
		return bottle.Dir()
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// No usable HOME: a bare root container. ~/.pkgx would be "/.pkgx"
		// or worse, and the prefix is the only location we actually know.
		return filepath.Join(prefix, "pkgs")
	}
	if under(prefix, home) {
		return bottle.Dir()
	}
	return filepath.Join(prefix, "pkgs")
}

// under reports whether path is dir or inside it, by path COMPONENT. A string
// prefix test would read /usr/locale as being under /usr/local, which is the
// bug every prefix match in this fleet has had once.
func under(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// ownerOf reports a path's owning uid and permission bits. A var so a test can
// describe ownership it cannot create: planting a file owned by another user
// requires root, and a test that needs root is a test that does not run.
var ownerOf = func(path string) (uid uint32, mode fs.FileMode, err error) {
	fi, err := osLstat(path)
	if err != nil {
		return 0, 0, err
	}
	u, ok := fileUID(fi)
	if !ok {
		// A platform whose FileInfo carries no uid (Windows, wasip1). The
		// mode still decides, and claiming uid 0 would make every path look
		// root-owned — a check that cannot see must not report "fine".
		return 0, fi.Mode(), errNoOwner
	}
	return u, fi.Mode(), nil
}

var errNoOwner = fmt.Errorf("this platform does not report file ownership")

// trustShimTargets refuses to write a shim whose target, or whose
// LD_LIBRARY_PATH, somebody other than the shim's owner can replace.
//
// The rule, stated once: a shim runs with the privileges of whoever invokes
// it, so every path it names must be writable only by root or by the user who
// owns the directory the shim goes in. Both halves matter —
//
//   - a FOREIGN OWNER means that user can overwrite the target outright;
//   - GROUP- or OTHER-WRITABLE means anyone can, owner notwithstanding.
//
// and so do the ANCESTORS: a 0777 directory anywhere above the target lets
// somebody move the whole subtree aside and put their own there, which no
// check of the leaf would see.
//
// Root is always in the allowed set because root can replace the shim itself;
// refusing on root-owned paths would reject every correct system install. The
// prefix's own owner is in the set because /usr/local/bin belongs to the admin
// user rather than to root on a stock macOS with Homebrew, and refusing there
// would be a false positive on the commonest system prefix there is.
func trustShimTargets(prefix, dir string, closure []bottle.Resolved) error {
	return trustStore(filepath.Join(prefix, "bin"), dir, closure)
}

// trustStore is trustShimTargets for any writer that records absolute store
// paths into something privileged. `pkgm service install` is the other one:
// it writes `ExecStart=<store>/<project>/v%i/bin/<command>` into
// /etc/systemd/system, which root runs at every boot — the same hole as the
// shim, with a worse blast radius, so it gets the same check with the unit
// directory as the owner reference.
func trustStore(ownerRef, dir string, closure []bottle.Resolved) error {
	refUID, _, err := ownerOf(ownerRef)
	if err != nil {
		// It does not exist yet, so we are about to create it and will own
		// it; or the platform does not report ownership, in which case euid
		// is the best statement available about who the owner will be.
		refUID = uint32(osGeteuid())
	}

	// What the shim trusts: the store root, every package prefix it execs
	// into, and every directory on the LD_LIBRARY_PATH it exports.
	trusted := map[string]bool{dir: true}
	for _, r := range closure {
		trusted[filepath.Join(dir, r.Project, "v"+r.Version.Raw)] = true
	}
	for _, p := range bottle.LibDirs(closure, dir) {
		trusted[p] = true
	}

	checked := map[string]bool{}
	for _, p := range sortedKeys(trusted) {
		for _, a := range ancestry(p) {
			if checked[a] {
				continue
			}
			checked[a] = true
			if err := trustOne(a, refUID, ownerRef); err != nil {
				return err
			}
		}
	}
	return nil
}

// trustOne is the whole judgement, on one path.
func trustOne(path string, refUID uint32, ownerRef string) error {
	uid, mode, err := ownerOf(path)
	if err == errNoOwner {
		// Ownership is unreadable here, so only the mode can be judged. Say
		// nothing about the owner rather than assume it is fine.
		uid = refUID
	} else if err != nil {
		// Absent is not untrusted: a lib dir listed for a package that has
		// none, or a store about to be created, is simply not there.
		return nil
	}
	// The suggestion has to be a path the reference's owner controls, which is
	// the store storeFor would have picked: beside the prefix, not in a home
	// directory sudo happened to keep.
	suggest := filepath.Join(filepath.Dir(ownerRef), "pkgs")
	if uid != 0 && uid != refUID {
		return fmt.Errorf(
			"refusing to record store paths under %s: %s is owned by uid %d, and %s belongs to uid %d — "+
				"uid %d could replace what gets run with root's privileges. "+
				"Use a store that uid %d controls: PKGX_DIR=%s",
			ownerRef, path, uid, ownerRef, refUID, uid, refUID, suggest)
	}
	if mode.IsDir() || mode.IsRegular() {
		if bad := mode.Perm() & 0o022; bad != 0 {
			who := "the group"
			if bad&0o002 != 0 {
				who = "anyone"
			}
			return fmt.Errorf(
				"refusing to record store paths under %s: %s is writable by %s (mode %04o) — "+
					"whoever can write it could replace what gets run. "+
					"Either tighten that path's permissions or use PKGX_DIR=%s",
				ownerRef, path, who, mode.Perm(), suggest)
		}
	}
	return nil
}

// ancestry is path and every directory above it, nearest first. The leaf alone
// is not enough: a writable directory anywhere above lets the whole subtree be
// swapped.
func ancestry(path string) []string {
	var out []string
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		out = append(out, p)
		if parent := filepath.Dir(p); parent == p {
			return out
		}
	}
}

// sortedKeys gives the trusted paths a stable order, so the path a refusal
// names does not move between runs over the same tree — two spellings of one
// problem read as two problems.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// seams, so the tests need neither root nor a particular machine.
var (
	osLstat   = os.Lstat
	osGeteuid = os.Geteuid
)

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-pkgx/bottle"
)

// resolved builds the one-package closure these tests shim.
func resolved(t *testing.T, project, version string) []bottle.Resolved {
	t.Helper()
	return []bottle.Resolved{{Project: project, Version: bottle.ParseVer(version)}}
}

// ⛔ TestRootStoreDoesNotFollowTheCallersHome is go-pkgx/pkgm#73's first half.
// macOS `sudo` keeps the caller's HOME, so ~/.pkgx resolved to an
// unprivileged user's home and root's shim pointed into it.
func TestRootStoreDoesNotFollowTheCallersHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PKGX_DIR", "")

	if got, want := storeFor("/usr/local"), "/usr/local/pkgs"; got != want {
		t.Errorf("storeFor(/usr/local) = %q, want %q — this is the escalation in #73", got, want)
	}
	if strings.HasPrefix(storeFor("/usr/local"), home) {
		t.Error("the system store is inside the caller's home")
	}
}

// TestAHomePrefixKeepsTheHomeStore: the ordinary case must not move. A plain
// `pkgm install` into ~/.local goes on using ~/.pkgx, or every existing
// install on every developer machine is suddenly in the wrong place.
func TestAHomePrefixKeepsTheHomeStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PKGX_DIR", "")

	want := filepath.Join(home, ".pkgx")
	if got := storeFor(filepath.Join(home, ".local")); got != want {
		t.Errorf("storeFor(~/.local) = %q, want %q", got, want)
	}
}

// TestPKGXDIRStillWins: it is explicit, it is what the refusals suggest, and a
// `FROM scratch` image sets it on purpose. A fix that overrode it would break
// the documented workaround for the very bug it fixes.
func TestPKGXDIRStillWins(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PKGX_DIR", "/opt/pkgx")

	if got := storeFor("/usr/local"); got != "/opt/pkgx" {
		t.Errorf("storeFor with PKGX_DIR set = %q, want /opt/pkgx", got)
	}
}

// TestAPrefixNamedAlongsideHomeIsNotInsideIt: /Users/x/.localextra is not
// under /Users/x/.local, and a string-prefix test would say it is. Every
// prefix match in this fleet has had this bug once.
func TestAPrefixNamedAlongsideHomeIsNotInsideIt(t *testing.T) {
	if under("/Users/x/.localextra", "/Users/x/.local") {
		t.Error(`under() matched on a string prefix, not a path component`)
	}
	if !under("/Users/x/.local/bin", "/Users/x/.local") {
		t.Error("under() missed a genuine descendant")
	}
	if !under("/Users/x/.local", "/Users/x/.local") {
		t.Error("under() says a directory is not under itself")
	}
}

// ⛔ TestAForeignOwnedStoreIsRefused is #73's second half, the invariant that
// actually closes the hole whatever the environment. The ownership is
// described rather than created: planting a file owned by another user needs
// root, and a test that needs root is a test that does not run.
func TestAForeignOwnedStoreIsRefused(t *testing.T) {
	store := "/Users/someone/.pkgx"
	foreign := filepath.Join(store, "go.dev", "v1.27.3")

	withOwners(t, func(path string) (uint32, fs.FileMode, error) {
		if under(path, store) {
			return 501, 0o755 | fs.ModeDir, nil // the invoking user's
		}
		return 0, 0o755 | fs.ModeDir, nil // root's
	})

	err := trustShimTargets("/usr/local", store, resolved(t, "go.dev", "1.27.3"))
	if err == nil {
		t.Fatal("a root prefix accepted a store owned by uid 501 — this is exactly #73")
	}
	for _, want := range []string{"501", "PKGX_DIR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	// It must name a path the reader can act on, not just complain.
	if !strings.Contains(err.Error(), store) && !strings.Contains(err.Error(), foreign) {
		t.Errorf("the refusal names no offending path: %v", err)
	}
}

// TestAUserOwnedStoreIsFineForThatUser: the same arrangement one level down is
// correct and must pass, or an ordinary `pkgm install` fails for everybody.
func TestAUserOwnedStoreIsFineForThatUser(t *testing.T) {
	withOwners(t, func(string) (uint32, fs.FileMode, error) {
		return 501, 0o755 | fs.ModeDir, nil
	})
	if err := trustShimTargets("/Users/someone/.local", "/Users/someone/.pkgx",
		resolved(t, "go.dev", "1.27.3")); err != nil {
		t.Errorf("a user's own prefix and store were refused: %v", err)
	}
}

// TestRootOwnedPathsAreAlwaysFine: root can replace the shim itself, so
// refusing on root-owned paths would reject every correct system install.
func TestRootOwnedPathsAreAlwaysFine(t *testing.T) {
	withOwners(t, func(string) (uint32, fs.FileMode, error) {
		return 0, 0o755 | fs.ModeDir, nil
	})
	if err := trustShimTargets("/usr/local", "/usr/local/pkgs",
		resolved(t, "go.dev", "1.27.3")); err != nil {
		t.Errorf("a root prefix with a root store was refused: %v", err)
	}
}

// TestAnAdminOwnedPrefixAcceptsARootStore. On a stock macOS with Homebrew,
// /usr/local/bin belongs to the admin user and not to root. Refusing there
// would be a false positive on the commonest system prefix there is.
func TestAnAdminOwnedPrefixAcceptsARootStore(t *testing.T) {
	withOwners(t, func(path string) (uint32, fs.FileMode, error) {
		if path == "/usr/local/bin" {
			return 501, 0o755 | fs.ModeDir, nil
		}
		return 0, 0o755 | fs.ModeDir, nil
	})
	if err := trustShimTargets("/usr/local", "/usr/local/pkgs",
		resolved(t, "go.dev", "1.27.3")); err != nil {
		t.Errorf("an admin-owned prefix with a root-owned store was refused: %v", err)
	}
}

// ⛔ TestAWorldWritableStoreIsRefused, with real files and no seam: chmod needs
// no privileges, so this half of the invariant is tested against the actual
// filesystem rather than against a description of one.
func TestAWorldWritableStoreIsRefused(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "usr", "local")
	store := filepath.Join(root, "store")
	pkg := filepath.Join(store, "go.dev", "v1.27.3")
	for _, d := range []string{filepath.Join(prefix, "bin"), filepath.Join(pkg, "lib")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	closure := resolved(t, "go.dev", "1.27.3")

	// Tight first: it has to PASS, or the refusal below proves nothing.
	if err := trustShimTargets(prefix, store, closure); err != nil {
		t.Fatalf("a 0755 tree was refused, so this test cannot attribute anything: %v", err)
	}

	if err := os.Chmod(pkg, 0o777); err != nil {
		t.Fatal(err)
	}
	err := trustShimTargets(prefix, store, closure)
	if err == nil {
		t.Fatal("a 0777 package directory was accepted; anyone could replace the binary the shim execs")
	}
	if !strings.Contains(err.Error(), "anyone") || !strings.Contains(err.Error(), pkg) {
		t.Errorf("the refusal does not say who or where: %v", err)
	}
}

// ⛔ TestAWritableAncestorIsRefused. The leaf is not enough: a 0777 directory
// above the target lets somebody move the subtree aside and put their own
// there, which no check of the leaf alone would see.
func TestAWritableAncestorIsRefused(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "usr", "local")
	store := filepath.Join(root, "loose", "store")
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store, "go.dev", "v1.27.3", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	closure := resolved(t, "go.dev", "1.27.3")
	if err := trustShimTargets(prefix, store, closure); err != nil {
		t.Fatalf("the tight tree was refused: %v", err)
	}

	loose := filepath.Join(root, "loose")
	if err := os.Chmod(loose, 0o777); err != nil {
		t.Fatal(err)
	}
	err := trustShimTargets(prefix, store, closure)
	if err == nil {
		t.Fatal("a 0777 ANCESTOR of the store was accepted")
	}
	if !strings.Contains(err.Error(), loose) {
		t.Errorf("the refusal blames the leaf rather than the writable ancestor: %v", err)
	}
}

// TestGroupWritableIsRefusedToo, and says "the group" rather than "anyone" —
// the two are different exposures and a reader acts on them differently.
func TestGroupWritableIsRefusedToo(t *testing.T) {
	root := t.TempDir()
	prefix := filepath.Join(root, "usr", "local")
	store := filepath.Join(root, "store")
	pkg := filepath.Join(store, "go.dev", "v1.27.3")
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pkg, 0o775); err != nil {
		t.Fatal(err)
	}
	err := trustShimTargets(prefix, store, resolved(t, "go.dev", "1.27.3"))
	if err == nil {
		t.Fatal("a group-writable package directory was accepted")
	}
	if !strings.Contains(err.Error(), "the group") {
		t.Errorf("it does not distinguish group from world: %v", err)
	}
}

// ⛔ TestAStickyDirectoryIsNotAnExposure. /tmp is mode 1777, and leaving this
// out turned every lane of this repository's CI red at once while passing
// locally — macOS t.TempDir() lands under /var/folders/<...>, which is 0700
// per user, so the ancestry walk never reached a world-writable directory,
// while on Linux it reaches /tmp on the second step.
//
// The sticky bit means only an entry's own owner may rename or delete it, so
// a store path already inside such a directory cannot be swapped. That is the
// whole reason /tmp is allowed to be world-writable.
func TestAStickyDirectoryIsNotAnExposure(t *testing.T) {
	root := t.TempDir()
	sticky := filepath.Join(root, "tmplike")
	store := filepath.Join(sticky, "store")
	prefix := filepath.Join(root, "usr", "local")
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store, "go.dev", "v1.27.3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	closure := resolved(t, "go.dev", "1.27.3")
	if err := trustShimTargets(prefix, store, closure); err != nil {
		t.Errorf("a store under a sticky 1777 directory was refused: %v", err)
	}

	// And the bit has no such meaning WITHOUT it: the same tree at 0777 is
	// still an exposure, which is what keeps this from being a blanket
	// exemption for anything world-writable.
	if err := os.Chmod(sticky, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := trustShimTargets(prefix, store, closure); err == nil {
		t.Error("the same directory at a plain 0777 was accepted")
	}
}

// TestAnAbsentPathIsNotUntrusted: a lib dir listed for a package that has
// none, or a store about to be created, is simply not there. Refusing on
// absence would make a first install impossible.
func TestAnAbsentPathIsNotUntrusted(t *testing.T) {
	root := t.TempDir()
	if err := trustShimTargets(filepath.Join(root, "usr", "local"),
		filepath.Join(root, "not-created-yet"), resolved(t, "go.dev", "1.27.3")); err != nil {
		t.Errorf("an absent store was refused: %v", err)
	}
}

// withOwners replaces the ownership reading for one test.
func withOwners(t *testing.T, f func(string) (uint32, fs.FileMode, error)) {
	t.Helper()
	prev := ownerOf
	ownerOf = f
	t.Cleanup(func() { ownerOf = prev })
}

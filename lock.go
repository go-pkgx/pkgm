package main

// Installing from a lock closes a loop that was open at the consumer end:
// `bk lock` wrote lockfiles and `bk factory --lock` could BUILD them, but
// nothing could INSTALL one. A lock nobody can install is a record of a build,
// not a way to repeat it — and repeating it is the only reason to write one.
//
// The defect it exists to stop is cheap to see and expensive to find: the same
// pantry commit resolved tcl to 9.0.4 and then to 9.1.0 four hours apart.
// Naming versions does not pin them; a lock does.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/go-pkgx/bottle"
)

// lockClosure reads a lockfile and returns EXACTLY what it pins, with no
// resolution of any kind.
//
// The order is whatever the file lists; `bk lock` writes its pins sorted, and
// nothing here depends on the sequence — a locked set is a set.
//
// ⛔ IT DOES NOT WALK THE DEPENDENCY GRAPH. `bottle.ResolveClosure` asks the
// registry what the newest satisfying version is today, which is the question
// a lock exists to stop anyone asking. Every pin is installed — not just the
// roots — because the roots are the only part of a lock that a resolver could
// reconstruct, and reconstructing it is what makes two installs differ.
func lockClosure(path, osn, arch string) ([]bottle.Resolved, error) {
	d, err := bottle.ReadLock(path)
	if err != nil {
		return nil, err
	}

	// ⛔ A `recipes` LOCK CANNOT BE INSTALLED, and saying so here is the whole
	// point of the field. Its versions answer "the newest these recipes can
	// BUILD", which is not "the newest the factory has PUBLISHED for this
	// platform" — measured upstream at 2 of 5 pins for curl.se on
	// linux/aarch64. Installing one would ask the registry for bottles that
	// were never built, and the failure would name a missing version rather
	// than the wrong KIND of lock.
	//
	// An empty field means `recipes`: that is what every lock written before
	// the field existed holds, so the default must refuse too.
	if mode := bottle.LockPinned(d); mode != bottle.PinsPublished {
		return nil, fmt.Errorf(
			"%s pins %q versions, which cannot be installed: they are the newest the recipes can BUILD, not the newest published for this platform — rewrite it with `bk lock -runnable`",
			path, mode)
	}

	// ⛔ AND A LOCK IS ONLY TRUE ON ITS OWN PLATFORM. The file records the one
	// it was taken on because the platform CHANGES the answer: a version
	// published for darwin/aarch64 need not exist for linux/x86-64. Installing
	// across platforms would either fail per-pin with a misleading "no such
	// version", or — worse, where the versions happen to coincide — succeed
	// and quietly give a set nobody locked.
	if want := osn + "/" + arch; d.Platform != "" && d.Platform != want {
		return nil, fmt.Errorf(
			"%s was taken on %s and this is %s: a lock pins what was published for ONE platform, so it cannot be installed here",
			path, d.Platform, want)
	}

	// A lock with no pins needs no check here: `bottle.ReadLock` already
	// refuses it with "no `locked` entries — this is not a lock". A guard of
	// my own would be unreachable code that reads as the thing keeping an
	// empty install from being reported as a success.

	out := make([]bottle.Resolved, 0, len(d.Pins))
	for _, p := range d.Pins {
		out = append(out, bottle.Resolved{Project: p.Project, Version: bottle.ParseVer(p.Version)})
	}
	return out, nil
}

// cmdInstallLock is `pkgm install --lock <file>`: install exactly the pinned
// set, then link and trust it the same way any other install does.
func cmdInstallLock(args []string, prefix string, f flags) error {
	if err := lockConflict(args, f); err != nil {
		return err
	}
	osn, arch := bottle.HostSlug()
	closure, err := lockClosure(f.lock, osn, arch)
	if err != nil {
		return err
	}
	dir := storeFor(prefix)
	if err := installLocked(os.Stdout, closure, dir); err != nil {
		return err
	}
	if err := trustShimTargets(prefix, dir, closure); err != nil {
		return err
	}
	n, err := bottle.StubBins(closure, dir, prefix)
	if err != nil {
		return err
	}
	fmt.Printf("linked %d binaries → %s\n", n, filepath.Join(prefix, "bin"))
	warnPath(prefix)
	return nil
}

// installLocked installs every pin and reports what each one did.
func installLocked(w io.Writer, closure []bottle.Resolved, dir string) error {
	for _, r := range closure {
		fresh, err := bottle.Install(r, dir)
		if err != nil {
			return fmt.Errorf("%s: %w", r.Project, err)
		}
		state := "cached"
		if fresh {
			state = "installed"
		}
		fmt.Fprintf(w, "  %-9s %s v%s\n", state, r.Project, r.Version.Raw)
	}
	return nil
}

// lockConflict rejects the combinations that would make `--lock` mean less
// than it says.
//
// ⛔ A LOCK IS THE WHOLE SET, not a floor to add to. Accepting extra packages
// beside it — named, or read from `-f` — would install something the lock does
// not describe while the command still reads as "install this lock", and the
// difference would show up as an unexplained version months later. Refusing is
// not a limitation; it is the guarantee.
func lockConflict(args []string, f flags) error {
	switch {
	case len(args) > 0:
		return fmt.Errorf("--lock %s installs exactly what the lock pins, so it cannot be combined with named packages (%v)", f.lock, args)
	case len(f.files) > 0:
		return fmt.Errorf("--lock %s installs exactly what the lock pins, so it cannot be combined with -f %s", f.lock, filepath.Base(f.files[0]))
	case f.pin:
		return fmt.Errorf("--lock %s is already pinned; -p/--pin would say the same thing about a different set", f.lock)
	case f.scratch:
		// ⛔ NOT a silent widening. `-s` materialises the implicit
		// glibc/libgcc_s/libstdc++/libatomic a FROM-scratch tree needs, which
		// are versions the lock does not name — so `--lock -s` would install
		// MORE than the lock and still call itself a locked install. Making a
		// scratch image reproducible needs those libraries pinned in the lock
		// itself, which is a change to `bk lock`, not a flag combination here.
		return fmt.Errorf("--lock %s cannot be combined with -s/--from-scratch: -s adds the implicit system libraries, which the lock does not pin, so the result would not be the locked set", f.lock)
	}
	return nil
}

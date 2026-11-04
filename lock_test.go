package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-zpkg/bottle"
)

// hostPlatform is what a lock taken here must name.
func hostPlatform() string {
	osn, arch := bottle.HostSlug()
	return osn + "/" + arch
}

// writeLock renders a lock with the LIBRARY'S OWN writer and puts it on disk.
//
// Hand-writing the HCL would test my idea of the format rather than the
// format: a fixture that drifts from `bk lock`'s output would keep passing
// while the real files stopped loading.
func writeLock(t *testing.T, d bottle.Lock) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.lock.hcl")
	if err := os.WriteFile(path, []byte(bottle.RenderLock(d)), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func publishedLock(pins ...bottle.LockPin) bottle.Lock {
	return bottle.Lock{
		Version:   1,
		Platform:  hostPlatform(),
		Generated: "2026-11-03T06:00:00Z",
		BK:        "v0.49.0",
		Pinned:    bottle.PinsPublished,
		Roots:     []string{"acme.org/tool"},
		Pins:      pins,
	}
}

// ⛔⛔ A `recipes` LOCK CANNOT BE INSTALLED, and the refusal must say which
// kind it got. Its versions are the newest the recipes can BUILD, which is not
// the newest PUBLISHED for a platform; installing one asks the registry for
// bottles that were never built, and the failure would name a missing version
// instead of the wrong kind of file.
func TestARecipesLockIsRefused(t *testing.T) {
	d := publishedLock(bottle.LockPin{Project: "acme.org/tool", Version: "1.0.0"})
	d.Pinned = bottle.PinsFromRecipes
	osn, arch := bottle.HostSlug()

	_, err := lockClosure(writeLock(t, d), osn, arch)
	if err == nil {
		t.Fatal("a recipes-mode lock was accepted for installation")
	}
	for _, want := range []string{"recipes", "BUILD", "bk lock -runnable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// ⛔ AND AN UNMARKED LOCK IS A `recipes` LOCK. Every lock written before the
// field existed has it empty, so a default that accepted them would install
// exactly the files most likely to be wrong.
func TestALockWithNoPinnedFieldIsTreatedAsRecipesAndRefused(t *testing.T) {
	d := publishedLock(bottle.LockPin{Project: "acme.org/tool", Version: "1.0.0"})
	d.Pinned = ""
	osn, arch := bottle.HostSlug()

	if _, err := lockClosure(writeLock(t, d), osn, arch); err == nil {
		t.Fatal("a lock with no `pinned` field was accepted for installation")
	}
}

// ⛔ A LOCK IS ONLY TRUE ON ITS OWN PLATFORM. A version published for one need
// not exist for another, and where the versions coincide the install would
// succeed while giving a set nobody locked.
func TestALockFromAnotherPlatformIsRefused(t *testing.T) {
	d := publishedLock(bottle.LockPin{Project: "acme.org/tool", Version: "1.0.0"})
	d.Platform = "linux/s390x"

	_, err := lockClosure(writeLock(t, d), "darwin", "aarch64")
	if err == nil {
		t.Fatal("a lock taken on another platform was accepted")
	}
	// It must name BOTH, or the reader cannot tell which end is wrong.
	for _, want := range []string{"linux/s390x", "darwin/aarch64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// ⛔ A LOCK THAT PINS NOTHING IS NOT AN EMPTY JOB — and the refusal comes from
// `bottle.ReadLock`, not from here.
//
// This test first asserted only that an error came back, and passed while a
// guard of mine never ran: the coverage profile showed the branch at count 0.
// The guard was unreachable and is gone; the message is asserted now, so the
// test says WHOSE refusal this is and fails if the library stops making it.
func TestALockWithNoPinsIsRefusedByTheReader(t *testing.T) {
	osn, arch := bottle.HostSlug()
	_, err := lockClosure(writeLock(t, publishedLock()), osn, arch)
	if err == nil {
		t.Fatal("a lock pinning nothing was accepted")
	}
	if !strings.Contains(err.Error(), "not a lock") {
		t.Errorf("the refusal did not come from the lock reader: %v", err)
	}
}

func TestAnUnreadableLockIsReported(t *testing.T) {
	osn, arch := bottle.HostSlug()
	if _, err := lockClosure(filepath.Join(t.TempDir(), "absent.hcl"), osn, arch); err == nil {
		t.Fatal("a missing lockfile was accepted")
	}
}

// ⛔⛔ EVERY PIN IS INSTALLED, NOT JUST THE ROOTS. The roots are the only part
// a resolver could reconstruct, and reconstructing them is exactly what makes
// two installs of the same lock differ. This lock names one root and pins
// three projects; all three must come back.
//
// Compared as a SET: `bk lock` writes its pins sorted, so asserting the file's
// sequence would be testing the writer's ordering rather than this reader's
// completeness — and it did fail that way first.
func TestEveryPinIsReturnedNotJustTheRoots(t *testing.T) {
	d := publishedLock(
		bottle.LockPin{Project: "acme.org/tool", Version: "1.0.0"},
		bottle.LockPin{Project: "acme.org/lib", Version: "2.3.4"},
		bottle.LockPin{Project: "zlib.net", Version: "1.3.2"},
	)
	osn, arch := bottle.HostSlug()

	got, err := lockClosure(writeLock(t, d), osn, arch)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"acme.org/tool": "1.0.0",
		"acme.org/lib":  "2.3.4",
		"zlib.net":      "1.3.2",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d pins, want %d: %v", len(got), len(want), got)
	}
	for _, r := range got {
		v, ok := want[r.Project]
		if !ok {
			t.Errorf("unexpected pin %s", r.Project)
			continue
		}
		if r.Version.Raw != v {
			t.Errorf("%s pinned at v%s, want v%s", r.Project, r.Version.Raw, v)
		}
		delete(want, r.Project)
	}
	for p := range want {
		t.Errorf("pin %s was dropped", p)
	}
}

// ⛔ A LOCK IS THE WHOLE SET. Accepting anything beside it would install what
// the lock does not describe while still reading as "install this lock".
func TestALockCannotBeCombinedWithAnythingThatWouldWidenIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		f    flags
		want string
	}{
		{"named packages", []string{"curl.se"}, flags{lock: "l.hcl"}, "named packages"},
		{"a package list", nil, flags{lock: "l.hcl", files: []string{"/tmp/pkgs.txt"}}, "-f"},
		{"--pin", nil, flags{lock: "l.hcl", pin: true}, "pinned"},
		{"--from-scratch", nil, flags{lock: "l.hcl", scratch: true}, "implicit system libraries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := lockConflict(tc.args, tc.f)
			if err == nil {
				t.Fatalf("--lock combined with %s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not explain itself (%q): %v", tc.want, err)
			}
		})
	}
	if err := lockConflict(nil, flags{lock: "l.hcl"}); err != nil {
		t.Errorf("a lock on its own was refused: %v", err)
	}
}

func TestParseArgsLockFlag(t *testing.T) {
	for _, argv := range [][]string{
		{"install", "--lock", "seed.lock.hcl"},
		{"install", "--lock=seed.lock.hcl"},
	} {
		pos, f := parseArgs(argv)
		if f.lock != "seed.lock.hcl" {
			t.Errorf("%v: lock = %q", argv, f.lock)
		}
		if len(pos) != 1 || pos[0] != "install" {
			t.Errorf("%v: positional = %v", argv, pos)
		}
	}
	// A dangling --lock takes no value rather than swallowing the next command.
	if _, f := parseArgs([]string{"install", "--lock"}); f.lock != "" {
		t.Errorf("a valueless --lock captured %q", f.lock)
	}
}

// End to end against the fake registry: a lock goes in, the pinned versions
// land in the store, and the binaries are linked.
func TestInstallFromALockInstallsThePinnedVersions(t *testing.T) {
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
	t.Setenv("ZPKG_DIR", dir)
	t.Setenv("PATH", filepath.Join(home, ".local", "bin"))
	prefix := filepath.Join(home, ".local")
	t.Setenv("PKGM_PREFIX", prefix)

	// ⛔ THE LOCK PINS THE OLDER VERSION ON PURPOSE. 2.0.0 is published and is
	// what any resolution would choose, so if the lock were resolved rather
	// than obeyed this test would install 2.0.0 and still look like a success.
	path := writeLock(t, publishedLock(
		bottle.LockPin{Project: "acme.org/tool", Version: "1.0.0"},
	))

	if err := dispatch("install", nil, flags{lock: path}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "acme.org", "tool", "v1.0.0")); err != nil {
		t.Errorf("the pinned version was not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "acme.org", "tool", "v2.0.0")); err == nil {
		t.Error("the newest version was installed: the lock was resolved, not obeyed")
	}
	if b, err := os.ReadFile(filepath.Join(prefix, "bin", "tool")); err != nil ||
		!strings.HasPrefix(string(b), "#!/bin/sh") {
		t.Errorf("the binary was not linked: %q err=%v", b, err)
	}
}

// A pin the registry cannot serve fails, and names the project — a locked
// install that cannot be satisfied is the one case where the lock is
// reporting real news, and "which project" is the whole of that news.
//
// ⛔ IT NAMES A PROJECT THE REGISTRY DOES NOT HAVE, not an absent VERSION of a
// present one: this fixture serves any version it is asked for under a known
// project, so an unpublished-version test would have passed while installing
// happily. It did, first time round.
func TestAPinThatCannotBeInstalledNamesTheProject(t *testing.T) {
	defer fakeServer(t, map[string]fakePkg{
		"acme.org/tool": {
			versions: []string{"1.0.0"},
			yaml:     "provides:\n  - bin/tool\n",
			files:    map[string]string{"bin/tool": "#!x\n"},
		},
	})()
	t.Setenv("ZPKG_DIR", t.TempDir())

	err := installLocked(io.Discard, []bottle.Resolved{
		{Project: "absent.example/nothing", Version: bottle.ParseVer("1.0.0")},
	}, t.TempDir())
	if err == nil {
		t.Fatal("a pin that is not published was reported as installed")
	}
	if !strings.Contains(err.Error(), "absent.example/nothing") {
		t.Errorf("the error does not name the project: %v", err)
	}
}

// cmdInstallLock refuses before it touches the network.
func TestInstallLockRefusesAConflictBeforeReadingAnything(t *testing.T) {
	err := cmdInstallLock([]string{"curl.se"}, t.TempDir(), flags{lock: "/nonexistent/l.hcl"})
	if err == nil {
		t.Fatal("a conflicting --lock invocation was accepted")
	}
	if !strings.Contains(err.Error(), "named packages") {
		t.Errorf("it failed for the wrong reason: %v", err)
	}
}

func TestInstallLockReportsAnUnreadableLock(t *testing.T) {
	if err := cmdInstallLock(nil, t.TempDir(), flags{lock: filepath.Join(t.TempDir(), "absent.hcl")}); err == nil {
		t.Fatal("a missing lockfile was accepted")
	}
}

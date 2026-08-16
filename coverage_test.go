package main

import (
	"errors"
	"testing"

	"github.com/go-pkgx/bottle"
)

func TestRunConfigWarning(t *testing.T) {
	old := configError
	defer func() { configError = old }()
	configError = func() error { return errors.New("bad config") }
	// The warning is emitted to stderr; the command still runs to completion.
	if code := run([]string{"--version"}); code != 0 {
		t.Errorf("--version with config warning code=%d", code)
	}
}

func TestRunEntry(t *testing.T) {
	bottle.Warn = nil
	if code := run([]string{"--version"}); code != 0 {
		t.Errorf("--version code=%d", code)
	}
	// run() installs the diagnostic sink: a soname the resolver cannot provide
	// must reach stderr, not wait for the installed program to fail to start.
	if bottle.Warn == nil {
		t.Error("run() must install bottle.Warn")
	}
	bottle.Warn("libnope.so.9 is NEEDED but nothing provides it") // must not panic
	if code := run([]string{"--help"}); code != 0 {
		t.Errorf("--help code=%d", code)
	}
	if code := run([]string{"help"}); code != 0 {
		t.Errorf("help code=%d", code)
	}
	if code := run(nil); code != 2 {
		t.Errorf("empty code=%d", code)
	}
	if code := run([]string{"frobnicate"}); code != 1 {
		t.Errorf("bad-cmd code=%d", code)
	}
	dir := t.TempDir()
	t.Setenv("PKGX_DIR", dir)
	if code := run([]string{"ls"}); code != 0 {
		t.Errorf("ls code=%d", code)
	}
}

func TestDispatchAliases(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PKGX_DIR", dir)
	// commands that only read the (empty) store must not error
	for _, c := range []string{"list", "ls", "outdated", "up", "update", "upgrade"} {
		if err := dispatch(c, nil, flags{}); err != nil {
			t.Errorf("dispatch %s: %v", c, err)
		}
	}
	// li / local-install with no args -> error path
	if err := dispatch("li", nil, flags{}); err == nil {
		t.Error("li empty should error")
	}
	if err := dispatch("x", nil, flags{}); err == nil {
		t.Error("x empty should error")
	}
}

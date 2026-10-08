package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-pkgx/bottle"
)

// cmdRun installs a package's closure (plus glibc on linux) and execs its
// binary through the pkgx loader — no shell, so it works on a FROM-scratch
// image where a bottle's /lib PT_INTERP does not exist.
func cmdRun(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("run: need a package")
	}
	project := args[0]
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	dir := bottle.Dir()
	// CompleteClosure installs the declared closure AND (on linux) the implicit
	// system-library bottles (glibc + libgcc_s/libstdc++ + libatomic) detected
	// from the bottles' own ELF NEEDED, so the package runs on FROM scratch.
	closure, err := bottle.CompleteClosure(map[string]string{project: "*"}, dir)
	if err != nil {
		return err
	}
	_, provides, err := bottle.FetchMeta(project)
	if err != nil {
		return err
	}
	prefix := bottle.PrefixOf(project, closure, dir)
	binPath := bottle.ResolveBinPath(filepath.Join(prefix, "bin", bottle.PrimaryBin(project, provides)))
	// BEFORE anything is launched, and before the wrapper detection below
	// installs a shell for a target that is not there.
	if err := checkRunnable(project, provides, binPath); err != nil {
		return err
	}
	libPath := bottle.LibPath(closure, dir)
	var shellPath string
	var pathDirs []string
	// If the target is a "#!/bin/sh" wrapper (pkgx wraps git, perl tools, …),
	// install the pkgx bash + coreutils and run it under those real tools —
	// pkgm is a package manager, not a shell. A real bash has the dash/bash
	// set -e semantics the wrappers rely on, and coreutils provides dirname etc.
	if bottle.GOOS() == "linux" && !bottle.IsELF(binPath) {
		if sh, err := bottle.CompleteClosure(map[string]string{"gnu.org/bash": "*", "gnu.org/coreutils": "*"}, dir); err == nil {
			closure = bottle.MergeClosures(closure, sh)
			libPath = bottle.LibPath(closure, dir)
			shellPath = bottle.FindClosureBin(closure, dir, "gnu.org/bash", "bash")
			if cu := bottle.FindClosureBin(closure, dir, "gnu.org/coreutils", "dirname"); cu != "" {
				pathDirs = append(pathDirs, filepath.Dir(cu))
			}
		}
	}
	var env []string
	if bottle.GOOS() == "windows" {
		// No LD_LIBRARY_PATH on Windows: DLLs resolve from the exe dir + PATH.
		// Put the closure's bin+lib dirs on PATH with the native separator.
		sep := string(os.PathListSeparator)
		parts := append(append([]string{}, pathDirs...), bottle.LibDirs(closure, dir)...)
		parts = append(parts, filepath.Dir(binPath))
		env = append(os.Environ(), "PATH="+strings.Join(parts, sep)+sep+os.Getenv("PATH"))
	} else {
		env = append(os.Environ(), "LD_LIBRARY_PATH="+libPath)
		if len(pathDirs) > 0 {
			env = append(env, "PATH="+strings.Join(pathDirs, ":")+":"+os.Getenv("PATH"))
		}
	}
	// On linux, best-effort place the pkgx loader at /lib/ld-linux and (for
	// wrapper scripts) bash at /bin/sh, so child processes and #!/bin/sh wrappers
	// resolve too (works when we are root on a writable rootfs, e.g. a scratch
	// image). Then, for an ELF, ALWAYS launch through the pkgx loader explicitly.
	//
	// The loader (ld-linux) and libc.so.6 are a matched pair, so we must NOT rely
	// on the host's canonical /lib64/ld-linux: on an unprivileged host we cannot
	// replace it, and an OLDER host loader cannot load a NEWER pkgx libc. Proven
	// on AlmaLinux 8 (host glibc 2.28): a direct exec of a pkgx tool fails with
	// "/lib64/ld-linux-x86-64.so.2: version `GLIBC_2.35' not found (required by
	// pkgx libc 2.44)", whereas the explicit pkgx loader runs it fine. This is
	// what lets pkgx bottles run on ANY host — no root, no from-scratch image,
	// bringing their own glibc (key for old/heterogeneous HPC login nodes).
	if bottle.GOOS() == "linux" {
		if loader := bottle.FindLoader(dir); loader != "" {
			bottle.SetupScratchRootfs(loader, shellPath)
			if bottle.IsELF(binPath) {
				argv := append([]string{loader, "--library-path", libPath, binPath}, rest...)
				return bottle.Exec(loader, argv, env)
			}
		}
	}
	argv := append([]string{binPath}, rest...)
	return bottle.Exec(binPath, argv, env)
}

// checkRunnable refuses, in words, a package that has nothing to run.
//
// ⛔ THE NAME OF THE BINARY IS A GUESS WHEN A PACKAGE DECLARES NOTHING.
// BinNames falls back to the project's leaf name for a recipe with no
// `provides:`, so `pkgm run zlib.net` looked for a program called "zlib.net"
// — a file nobody had ever mentioned. Handing that path to exec produced the
// kernel's own answer and nothing else:
//
//	pkgm: no such file or directory
//
// That sentence names neither the package, nor the file, nor the only fact
// that matters: zlib is a LIBRARY. Measured 2026-10-08 in a FROM-scratch
// image, after the whole closure had already been downloaded — so the reader
// is told at the very end, in the most opaque way available, something that
// was knowable from the recipe.
//
// It still refuses at the end, deliberately. An empty `provides:` could be
// read from the recipe before a single byte is fetched, but it does not mean
// "no command" on its own — several recipes ship a binary they never declare,
// and the guessed name finds it. The only honest question is whether the file
// is THERE, and that one cannot be asked before the bottle is unpacked.
//
// The two cases are kept apart because they ask for different things. An
// empty `provides:` is the package working as intended and the REQUEST being
// wrong. A declared command that is missing from the bottle is the package,
// or our build of it, being wrong — so that one names the path, which is what
// somebody would need in order to go and look.
func checkRunnable(project string, provides []string, binPath string) error {
	if _, err := os.Stat(binPath); err == nil {
		return nil
	}
	if len(provides) == 0 {
		return fmt.Errorf("%s provides no command: it is a library, not a program — `pkgm install %s` installs its files, but there is nothing to run", project, project)
	}
	return fmt.Errorf("%s declares %s, but %s is not in the installed bottle", project, strings.Join(provides, ", "), binPath)
}

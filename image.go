package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// cmdImage writes, to stdout, a `FROM scratch` Containerfile that installs the
// package's whole runtime closure with pkgm ITSELF, from inside the image, and
// runs it shell-free.
//
// The output is the vendor-neutral OCI "Containerfile" — identical syntax to a
// Dockerfile; podman/buildah pick it up by name, and docker builds it with
// `-f Containerfile`.
//
// pkgm is a static CGO_ENABLED=0 binary that already runs on `FROM scratch`, so
// it bootstraps the entire userland from within the image: the `RUN` step pulls
// the closure over HTTPS (pkgm's embedded CA + net/http) straight into the
// image and wires the pkgx loader (bottle.SetupScratchRootfs). There is no
// host-side rootfs staging and no manual loader symlink — the `RUN` step IS the
// from-scratch install (and, in CI, the proof that the published bottles are
// self-sufficient with no system libc).
//
// The subcommand only emits the Containerfile: pkgm is a package manager, not a
// container-build driver. The build context must contain the target-arch `pkgm`
// binary as `pkgm`. Build + smoke-test (docker; podman is `podman build .`):
//
//	pkgm image lz4.org > Containerfile
//	cp "$(command -v pkgm)" pkgm
//	docker build -f Containerfile -t scratch-lz4 .
//	docker run --rm scratch-lz4 --version
func cmdImage(args []string) error {
	fs := flag.NewFlagSet("image", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	overlay := fs.String("overlay", "", "bake PKGX_PANTRY_OVERLAY=<url> into the image (corrected recipes consulted before the upstream pantry)")
	glibc := fs.String("glibc", "", "pin glibc to an exact version in the image, e.g. -glibc 2.28 (default: newest)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return writeImage(os.Stdout, fs.Args(), *overlay, *glibc)
}

// writeImage is the testable core of cmdImage (stdout injected).
func writeImage(w io.Writer, args []string, overlay, glibc string) error {
	if len(args) == 0 {
		return fmt.Errorf("image: need a package")
	}
	project := args[0]
	// The RUN installs the package's from-scratch closure. Pinning glibc is just
	// an extra explicit root: CompleteClosure adds the implicit glibc only when
	// none is already in the closure, so a pinned gnu.org/glibc@=<ver> wins and
	// the image ships exactly that glibc (deterministic for a known HPC kernel).
	run := []string{"/pkgm", "install", "-s", project}
	if glibc != "" {
		run = append(run, "gnu.org/glibc@="+strings.TrimPrefix(glibc, "="))
	}
	// exec-form (JSON) RUN/ENTRYPOINT need no shell — essential on scratch,
	// where there is no /bin/sh. json.Marshal escapes the args safely.
	var b strings.Builder
	fmt.Fprintln(&b, "FROM scratch")
	fmt.Fprintln(&b, "ENV PKGX_DIR=/pkgx")
	fmt.Fprintln(&b, "ENV HOME=/root")
	if overlay != "" {
		fmt.Fprintln(&b, "ENV PKGX_PANTRY_OVERLAY="+overlay)
	}
	fmt.Fprintln(&b, "COPY pkgm /pkgm")
	fmt.Fprintln(&b, "RUN "+execForm(run...))
	fmt.Fprintln(&b, "ENTRYPOINT "+execForm("/pkgm", "run", project, "--"))
	_, err := io.WriteString(w, b.String())
	return err
}

// execForm renders argv as a Docker exec-form JSON array (no shell involved).
// json.Marshal of a []string cannot fail, so the error is discarded — leaving
// no unreachable branch to defeat the 100%-coverage rule.
func execForm(argv ...string) string {
	data, _ := json.Marshal(argv)
	return string(data)
}

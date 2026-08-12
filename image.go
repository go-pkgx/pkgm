package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// cmdImage writes, to stdout, a `FROM scratch` Dockerfile that installs the
// package's whole runtime closure with pkgm ITSELF, from inside the image, and
// runs it shell-free.
//
// pkgm is a static CGO_ENABLED=0 binary that already runs on `FROM scratch`, so
// it bootstraps the entire userland from within the image: the `RUN` step pulls
// the closure over HTTPS (pkgm's embedded CA + net/http) straight into the
// image and wires the pkgx loader (bottle.SetupScratchRootfs). There is no
// host-side rootfs staging and no manual loader symlink — the `RUN` step IS the
// from-scratch install (and, in CI, the proof that the published bottles are
// self-sufficient with no system libc).
//
// The subcommand only emits the Dockerfile: pkgm is a package manager, not a
// docker driver. The build context must contain the target-arch `pkgm` binary
// as `pkgm`. Build + smoke-test:
//
//	pkgm image lz4.org > Dockerfile
//	cp "$(command -v pkgm)" pkgm
//	docker build -t scratch-lz4 .
//	docker run --rm scratch-lz4 --version
func cmdImage(args []string) error {
	return writeImage(os.Stdout, args)
}

// writeImage is the testable core of cmdImage (stdout injected).
func writeImage(w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("image: need a package")
	}
	project := args[0]
	// exec-form (JSON) RUN/ENTRYPOINT need no shell — essential on scratch,
	// where there is no /bin/sh. json.Marshal escapes the project safely.
	var b strings.Builder
	fmt.Fprintln(&b, "FROM scratch")
	fmt.Fprintln(&b, "ENV PKGX_DIR=/pkgx")
	fmt.Fprintln(&b, "ENV HOME=/root")
	fmt.Fprintln(&b, "COPY pkgm /pkgm")
	fmt.Fprintln(&b, "RUN "+execForm("/pkgm", "install", "-s", project))
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

package main

import (
	"errors"
	"strings"
	"testing"
)

func TestWriteImage(t *testing.T) {
	var b strings.Builder
	if err := writeImage(&b, []string{"lz4.org"}, "", ""); err != nil {
		t.Fatalf("writeImage: %v", err)
	}
	got := b.String()
	for _, want := range []string{
		"FROM scratch\n",
		"ENV PKGX_DIR=/pkgx\n",
		"ENV HOME=/root\n",
		"COPY pkgm /pkgm\n",
		`RUN ["/pkgm","install","-s","lz4.org"]` + "\n",
		`ENTRYPOINT ["/pkgm","run","lz4.org","--"]` + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Containerfile missing %q\n---\n%s", want, got)
		}
	}
	// without -overlay, no PKGX_PANTRY_OVERLAY line
	if strings.Contains(got, "PKGX_PANTRY_OVERLAY") {
		t.Errorf("unexpected overlay env:\n%s", got)
	}
}

// -overlay bakes an ENV PKGX_PANTRY_OVERLAY line before COPY.
func TestWriteImageOverlay(t *testing.T) {
	var b strings.Builder
	url := "https://raw.githubusercontent.com/go-pkgx/pantry-overlay/main/projects"
	if err := writeImage(&b, []string{"curl.se"}, url, ""); err != nil {
		t.Fatalf("writeImage: %v", err)
	}
	got := b.String()
	if !strings.Contains(got, "ENV PKGX_PANTRY_OVERLAY="+url+"\n") {
		t.Errorf("overlay env missing:\n%s", got)
	}
	if strings.Index(got, "PKGX_PANTRY_OVERLAY") > strings.Index(got, "COPY pkgm") {
		t.Errorf("overlay env must precede COPY:\n%s", got)
	}
}

// -glibc bakes the pin as PKGX_GLIBC, so it holds for the install RUN AND for
// every later `pkgm run` in the image; a leading "=" is not doubled.
func TestWriteImageGlibcPin(t *testing.T) {
	for _, in := range []string{"2.28", "=2.28"} {
		var b strings.Builder
		if err := writeImage(&b, []string{"lz4.org"}, "", in); err != nil {
			t.Fatalf("writeImage glibc=%q: %v", in, err)
		}
		got := b.String()
		if !strings.Contains(got, "ENV PKGX_GLIBC=2.28\n") {
			t.Errorf("glibc=%q: pin env missing:\n%s", in, got)
		}
		if strings.Index(got, "PKGX_GLIBC") > strings.Index(got, "COPY pkgm") {
			t.Errorf("pin env must precede COPY (it governs the RUN):\n%s", got)
		}
	}
	// unpinned: no stray env
	var b strings.Builder
	if err := writeImage(&b, []string{"lz4.org"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "PKGX_GLIBC") {
		t.Errorf("unpinned image carries a glibc pin:\n%s", b.String())
	}
}

// A project name with a JSON metacharacter must be escaped, not injected.
func TestWriteImageEscapesProject(t *testing.T) {
	var b strings.Builder
	if err := writeImage(&b, []string{`x"y`}, "", ""); err != nil {
		t.Fatalf("writeImage: %v", err)
	}
	if !strings.Contains(b.String(), `"x\"y"`) {
		t.Errorf("project not JSON-escaped:\n%s", b.String())
	}
}

func TestWriteImageNoArgs(t *testing.T) {
	var b strings.Builder
	if err := writeImage(&b, nil, "", ""); err == nil {
		t.Error("writeImage(nil) should error")
	}
	if b.Len() != 0 {
		t.Errorf("no Containerfile should be written on error, got %q", b.String())
	}
}

// failWriter fails on the first write, covering writeImage's io error branch.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestWriteImageWriteError(t *testing.T) {
	if err := writeImage(failWriter{}, []string{"lz4.org"}, "", ""); err == nil {
		t.Error("writeImage should propagate a write error")
	}
}

// cmdImage is the flag-parsing + stdout-wired wrapper.
func TestCmdImage(t *testing.T) {
	if err := cmdImage([]string{"lz4.org"}); err != nil {
		t.Errorf("cmdImage: %v", err)
	}
	if err := cmdImage([]string{"-overlay", "https://ov.example/projects", "curl.se"}); err != nil {
		t.Errorf("cmdImage -overlay: %v", err)
	}
	if err := cmdImage(nil); err == nil {
		t.Error("cmdImage(nil) should error")
	}
	if err := cmdImage([]string{"-nope"}); err == nil {
		t.Error("cmdImage with an unknown flag should error")
	}
}

func TestDispatchImage(t *testing.T) {
	if err := dispatch("image", []string{"lz4.org"}, flags{}); err != nil {
		t.Errorf("dispatch image: %v", err)
	}
	if err := dispatch("image", nil, flags{}); err == nil {
		t.Error("dispatch image with no pkg should error")
	}
}

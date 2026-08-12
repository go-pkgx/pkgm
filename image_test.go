package main

import (
	"errors"
	"strings"
	"testing"
)

func TestWriteImage(t *testing.T) {
	var b strings.Builder
	if err := writeImage(&b, []string{"lz4.org"}); err != nil {
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
			t.Errorf("Dockerfile missing %q\n---\n%s", want, got)
		}
	}
}

// A project name with a JSON metacharacter must be escaped, not injected, into
// the exec-form arrays.
func TestWriteImageEscapesProject(t *testing.T) {
	var b strings.Builder
	if err := writeImage(&b, []string{`x"y`}); err != nil {
		t.Fatalf("writeImage: %v", err)
	}
	if !strings.Contains(b.String(), `"x\"y"`) {
		t.Errorf("project not JSON-escaped:\n%s", b.String())
	}
}

func TestWriteImageNoArgs(t *testing.T) {
	var b strings.Builder
	if err := writeImage(&b, nil); err == nil {
		t.Error("writeImage(nil) should error")
	}
	if b.Len() != 0 {
		t.Errorf("no Dockerfile should be written on error, got %q", b.String())
	}
}

// failWriter fails on the first write, covering writeImage's io error branch.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestWriteImageWriteError(t *testing.T) {
	if err := writeImage(failWriter{}, []string{"lz4.org"}); err == nil {
		t.Error("writeImage should propagate a write error")
	}
}

// cmdImage is the stdout-wired wrapper; exercise both its success and error
// paths (it delegates to writeImage on os.Stdout).
func TestCmdImage(t *testing.T) {
	if err := cmdImage([]string{"lz4.org"}); err != nil {
		t.Errorf("cmdImage: %v", err)
	}
	if err := cmdImage(nil); err == nil {
		t.Error("cmdImage(nil) should error")
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

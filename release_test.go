package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ⛔ EVERY PUBLISHED FILE CARRIES PROVENANCE.
//
// SHA256SUMS is a checksum of itself: it says the binaries match a manifest
// published beside them, and whoever replaced the binaries would replace the
// manifest. The half that cannot be forged from the release page is GitHub's
// Sigstore-signed statement that THIS workflow built THAT file from THAT
// commit.
//
// Measured 2026-10-20 on go-pkgx/pkgx v0.23.1, with go-fleettools' relverify:
// twelve files, every checksum right, and every attestation lookup
// `HTTP 404: Not Found`. The same was true of this repository, of go-pkgx/bk
// and of go-pkgx/mirror — releases that verify as far as a manifest can take
// them and no further, with nothing saying so, because a missing attestation
// is an ABSENCE and a release page looks identical with and without one.
//
// The three permissions are part of the check and not decoration: the action
// is a no-op failure without `id-token: write` and `attestations: write`, and
// `contents: write` is what uploads the files, so a job that lost one of them
// would publish an unattested release and stay green on the other two.
func TestTheReleaseAttestsWhatItPublishes(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Skipf("no release workflow to read here: %v", err)
	}
	wf := string(b)
	for _, want := range []string{
		"actions/attest-build-provenance@",
		"id-token: write",
		"attestations: write",
		"contents: write",
		"dist/pkgm-*",
		"dist/SHA256SUMS",
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("release.yml is missing %q — the release would publish unattested files", want)
		}
	}
	// ⛔ ORDER, and it is the whole value of the step. Attesting before the
	// stamp guard would sign a binary the guard has not yet checked;
	// publishing before attesting would put files on the release page that
	// nothing has signed.
	guard := strings.Index(wf, "Refuse a dirty or mis-stamped build")
	attest := strings.Index(wf, "actions/attest-build-provenance@")
	publish := strings.Index(wf, "Publish GitHub Release")
	if guard < 0 || attest < 0 || publish < 0 {
		t.Fatalf("release.yml has lost one of its three steps: guard=%d attest=%d publish=%d", guard, attest, publish)
	}
	if !(guard < attest && attest < publish) {
		t.Errorf("the steps run guard=%d attest=%d publish=%d; attestation must sit between the guard and the publish", guard, attest, publish)
	}
}

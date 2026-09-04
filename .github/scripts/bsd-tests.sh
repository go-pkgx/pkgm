#!/bin/sh
# Cross-compile this module's test binaries for a BSD, then run them there.
#
# The toolchain never leaves the Linux runner: bk's go.mod names a Go version
# no BSD package set carries, and installing a toolchain inside a VM would make
# the lane depend on what a package mirror happens to hold today. So the same
# idiom the qemu lanes use — build here, run there — with the VM providing only
# a kernel and a libc, which is the whole point of the exercise.
#
# Usage:
#   bsd-tests.sh build <goos>   on the runner, before the VM starts
#   bsd-tests.sh run            inside the VM
#
# Test binaries are written NEXT TO their package, because a test that opens
# testdata/ expects to run from its own directory and there is no reason to
# make the lane the one place that is not true.
set -eu

case "${1:-}" in
build)
	goos="${2:?usage: bsd-tests.sh build <goos>}"
	: >bsd-tests.list
	for pkg in $(go list -f '{{if .TestGoFiles}}{{.ImportPath}}{{end}}' ./...); do
		dir=$(go list -f '{{.Dir}}' "$pkg")
		rel=${dir#"$PWD"/}
		[ "$rel" = "$dir" ] && rel=.
		GOOS="$goos" GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$rel/bsd.test" "$pkg"
		echo "$rel" >>bsd-tests.list
	done
	wc -l <bsd-tests.list | tr -d ' ' | sed 's/$/ test binaries built/'
	;;
run)
	fail=0
	while read -r rel; do
		echo "=== $rel"
		# -test.count=1: a cached PASS cannot stand in for a run that never
		# happened on this kernel. There is no cache here, but the day someone
		# adds one this line is what keeps the lane honest.
		(cd "$rel" && ./bsd.test -test.count=1) || fail=1
	done <bsd-tests.list
	exit "$fail"
	;;
*)
	echo "usage: bsd-tests.sh build <goos> | bsd-tests.sh run" >&2
	exit 2
	;;
esac

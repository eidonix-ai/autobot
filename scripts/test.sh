#!/bin/sh
# Autobot's tests, in two sizes.
#
#   scripts/test.sh quick [package…]   while iterating: the packages named (default: those with
#                                      uncommitted changes), no browser tests
#   scripts/test.sh full               before a release: vet, every test, the browser tests, and
#                                      the race detector on the core packages
#
# deploy/install.sh runs the full set first and does not deploy on a failure.
set -eu
cd "$(dirname "$0")/.."
mode=${1:-quick}
[ $# -gt 0 ] && shift

case "$mode" in
quick)
	pkgs="$*"
	if [ -z "$pkgs" ]; then
		# The packages of the Go files changed since the last commit.
		pkgs=$(git status --porcelain -- '*.go' | awk '{print $NF}' | xargs -n1 dirname 2>/dev/null | sort -u | sed 's|^|./|')
	fi
	if [ -z "$pkgs" ]; then
		echo "No changed Go packages; name some: scripts/test.sh quick ./internal/workflow"
		exit 0
	fi
	echo "quick: $pkgs"
	go vet $pkgs
	go test -p 1 $pkgs
	;;
full)
	echo "full: vet"
	go vet ./...
	go vet -tags browser ./internal/web
	echo "full: tests"
	go test -p 1 -count=1 ./...
	echo "full: browser tests"
	go test -p 1 -count=1 -tags browser -run Browser ./internal/web
	echo "full: race detector on the core packages"
	go test -p 1 -count=1 -race ./internal/agent ./internal/store ./internal/workflow
	echo "full: passed"
	;;
*)
	echo "usage: scripts/test.sh quick [package…] | full" >&2
	exit 2
	;;
esac

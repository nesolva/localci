#!/bin/sh
# Fails when docs/config.md differs from what its generator writes now.
# Compares against the working tree, not the index, so it behaves the same on a
# clean checkout and on uncommitted changes.
set -eu
out="$(mktemp)"
trap 'rm -f "$out"' EXIT
(cd internal/config && go run ./gen "$out")
if ! diff -u docs/config.md "$out"; then
  echo "docs/config.md is out of date: run 'make generate'" >&2
  exit 1
fi

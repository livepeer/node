#!/bin/sh
set -eu

# The digest-pinned Docker builder is the compiler version source for CI/releases.
version=$(sed -nE 's/^FROM .*golang:([0-9]+\.[0-9]+\.[0-9]+)@sha256:[0-9a-f]{64} AS build$/\1/p' docker/Dockerfile)
if [ -z "$version" ] || [ "$(printf '%s\n' "$version" | wc -l)" -ne 1 ]; then
  echo 'Expected one digest-pinned golang builder in docker/Dockerfile' >&2
  exit 1
fi
printf '%s\n' "$version"

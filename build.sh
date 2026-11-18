#!/bin/sh
# Builds the static forge binary and checks it against the entry caps.
#
# The binary is the whole entry: CGO off, trimmed paths, stripped. It must
# stay under 8 MiB (the per-file entry cap). gofmt and go vet run at the
# end; vet is expected to fail until every feature package has landed, so
# read its errors as missing-dependency vs real.
set -eu
cd "$(dirname "$0")"
export PATH="$HOME/.local/go/bin:$PATH"
export CGO_ENABLED=0

go build -trimpath -ldflags "-s -w" -o forge .

size=$(stat -c%s forge)
echo "forge binary: $size bytes"
if [ "$size" -ge 8388608 ]; then
  echo "build.sh: binary is >= 8 MiB ($size bytes), over the per-file entry cap" >&2
  exit 1
fi

echo "--- gofmt ---"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "files needing gofmt:"
  echo "$unformatted"
else
  echo "gofmt clean"
fi

echo "--- go vet ---"
go vet ./...

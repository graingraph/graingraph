#!/usr/bin/env bash
set -eux

# Skip on Windows where gcc may not be available
if [[ "$OSTYPE" == "msys"* || "$OSTYPE" == "win32" ]]; then
  echo "Skipping C build on Windows"
  exit 0
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
CWD="$REPO_ROOT"
mkdir -p "$REPO_ROOT/build"

# fastjson
gcc -O2 -fPIC -c "$CWD/c/fastjson.c" -o "$CWD/build/fastjson.o"
ar rcs "$CWD/c/libfastjson.a" "$CWD/build/fastjson.o"

# vector
gcc -O2 -fPIC -c "$CWD/c/vector_kernels.c" -o "$CWD/build/vector_kernels.o"
ar rcs "$CWD/c/libvector.a" "$CWD/build/vector_kernels.o"

# evalcore
gcc -O2 -fPIC -c "$CWD/c/eval_core.c" -o "$CWD/build/eval_core.o"
ar rcs "$CWD/c/libevalcore.a" "$CWD/build/eval_core.o"

echo "C libs built"

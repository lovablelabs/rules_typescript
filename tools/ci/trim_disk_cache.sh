#!/usr/bin/env bash
# trim_disk_cache.sh -- delete the least recently used entries of a Bazel disk
# cache until it is at most MAX_BYTES.
#
# The disk cache CI saves is restored, added to and saved again, so without a
# bound it only grows, and the Actions cache it lives in holds 10 GB in all.
# Bazel's own collector (--experimental_disk_cache_gc_max_size) runs only after
# the server has idled for --experimental_disk_cache_gc_idle_delay, which a CI
# job never does before its cache is saved.
#
# Bazel sets an entry's mtime whenever it stores or retrieves it, documents
# trimming by an external process as supported, and treats an action result
# whose outputs are gone as a miss -- so deleting by oldest mtime is the same
# policy its collector applies.
#
#   tools/ci/trim_disk_cache.sh ~/.cache/bazel-disk 2147483648

set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 DIR MAX_BYTES" >&2
  exit 2
fi
dir="$1"
max="$2"

if [ ! -d "$dir" ]; then
  echo "trim_disk_cache: $dir does not exist"
  exit 0
fi

# mtime, size and path of every file, newest first.
list() {
  if stat -c '%Y' "$dir" > /dev/null 2>&1; then
    find "$dir" -type f -exec stat -c '%Y %s %n' {} +
  else
    find "$dir" -type f -exec stat -f '%m %z %N' {} +
  fi | LC_ALL=C sort -k1,1nr
}

list | awk -v max="$max" '
  { total += $2 }
  total > max {
    path = $0
    sub(/^[^ ]+ [^ ]+ /, "", path)
    print path
    freed += $2
    n++
  }
  END {
    printf "trim_disk_cache: %.0f MB, cap %.0f MB, deleting %d entries (%.0f MB)\n",
      total / 1048576, max / 1048576, n, freed / 1048576 > "/dev/stderr"
  }
' | while IFS= read -r path; do
  rm -f -- "$path"
done

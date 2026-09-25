#!/bin/bash

# Flatten files below one or more Google Drive folders without overwriting data.
# Compatible with the Bash 3.2 shipped with macOS.

set -u

execute=false
roots=()

usage() {
  cat <<'EOF'
Usage:
  migrate-pictures-tool.sh [--execute] DIRECTORY [DIRECTORY ...]

Without --execute, print the planned moves without changing files.
Supply one or more directories. Each directory is flattened independently.

Only files below subdirectories are moved. Files already at the root stay there.
If a filename already exists or is planned, __flat_N is added before its extension.
No existing file is overwritten. Empty subdirectories are removed after execution.

Examples:
  ./scripts/migrate-pictures-tool.sh "/path/to/folder"
  ./scripts/migrate-pictures-tool.sh --execute "/path/to/folder"
  ./scripts/migrate-pictures-tool.sh --execute "/path/one" "/path/two"
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --execute)
      execute=true
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      while [ "$#" -gt 0 ]; do
        roots+=("$1")
        shift
      done
      break
      ;;
    -*)
      printf 'ERROR: unknown option: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
    *)
      roots+=("$1")
      ;;
  esac
  shift
done

if [ "${#roots[@]}" -eq 0 ]; then
  printf 'ERROR: at least one DIRECTORY is required.\n' >&2
  usage >&2
  exit 2
fi

quote() {
  printf "'%s'" "$1"
}

is_reserved() {
  [ -e "$1" ] || /usr/bin/grep -Fqx "$1" "$reserved_file" 2>/dev/null
}

choose_destination() {
  local root=$1
  local base=$2
  local stem ext candidate number

  candidate="$root/$base"
  if ! is_reserved "$candidate"; then
    printf '%s\n' "$candidate"
    return
  fi

  case "$base" in
    *.*)
      stem=${base%.*}
      ext=.${base##*.}
      ;;
    *)
      stem=$base
      ext=
      ;;
  esac

  number=1
  while :; do
    candidate="$root/${stem}__flat_${number}${ext}"
    if ! is_reserved "$candidate"; then
      printf '%s\n' "$candidate"
      return
    fi
    number=$((number + 1))
  done
}

mode=DRY-RUN
if $execute; then
  mode=EXECUTE
fi
printf 'Mode: %s\n' "$mode"

found_root=false
overall_failed=0

for requested_root in "${roots[@]}"; do
  if [ ! -d "$requested_root" ]; then
    {
      printf 'WARNING: directory not found, skipped: '
      quote "$requested_root"
      printf '\n'
      if [[ "$requested_root" == *\\* ]]; then
        printf 'HINT: the path contains a literal backslash. Inside double quotes, do not escape spaces or underscores.\n'
      fi
    } >&2
    continue
  fi

  found_root=true
  root=$(cd "$requested_root" 2>/dev/null && pwd -P)
  if [ -z "$root" ] || [ "$root" = "/" ]; then
    printf 'ERROR: refusing unsafe root: %s\n' "$requested_root" >&2
    overall_failed=1
    continue
  fi

  file_list=$(mktemp -t photos-to-drive-flatten-files.XXXXXX) || exit 1
  reserved_file=$(mktemp -t photos-to-drive-flatten-reserved.XXXXXX) || {
    rm -f "$file_list"
    exit 1
  }

  # Ignore Finder metadata. Symlinks and files already at root are not selected.
  if ! find "$root" -mindepth 2 -type f ! -name '.DS_Store' -print0 >"$file_list"; then
    printf 'ERROR: failed to enumerate files below: %s\n' "$root" >&2
    rm -f "$file_list" "$reserved_file"
    overall_failed=1
    continue
  fi

  planned=0
  moved=0
  failed=0
  renamed=0

  printf '\nRoot: '
  quote "$root"
  printf '\n'

  while IFS= read -r -d '' source; do
    base=${source##*/}
    destination=$(choose_destination "$root" "$base")
    printf '%s\n' "$destination" >>"$reserved_file"
    planned=$((planned + 1))

    if [ "$destination" != "$root/$base" ]; then
      renamed=$((renamed + 1))
    fi

    printf 'MOVE '
    quote "$source"
    printf ' -> '
    quote "$destination"
    printf '\n'

    if $execute; then
      # -n is an additional race-safe guard against overwriting a file created
      # after the destination was selected.
      if mv -n "$source" "$destination" && [ ! -e "$source" ]; then
        moved=$((moved + 1))
      else
        printf 'ERROR: move failed: ' >&2
        quote "$source" >&2
        printf '\n' >&2
        failed=$((failed + 1))
        overall_failed=1
      fi
    fi
  done <"$file_list"

  if $execute; then
    # Remove only directories that became empty; the selected root itself is retained.
    find "$root" -depth -mindepth 1 -type d -empty -delete
  fi

  printf 'Summary: planned=%d; moved=%d; collision-renamed=%d; failed=%d\n' \
    "$planned" "$moved" "$renamed" "$failed"

  rm -f "$file_list" "$reserved_file"
done

if ! $found_root; then
  printf 'ERROR: none of the requested directories exist.\n' >&2
  exit 1
fi

if ! $execute; then
  printf '\nDry run only. Re-run with --execute to move the files.\n'
fi

exit "$overall_failed"

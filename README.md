# photos-to-drive

[日本語](README.ja.md)

A Go CLI for macOS that processes locally available originals from an Apple Photos library one file at a time:

**create a temporary copy → upload it to Google Drive → verify its size and MD5 → remove the temporary copy**

The tool does not modify the source Photos library, iCloud settings, or photos on an iPhone. It does not reduce the size of the source library.

## Scope and limitations

- The CLI reads local images and videos under `originals/` in a current Photos library. It does not use PhotoKit or the Photos app's Export command. See `media` in `main.go` for supported extensions.
- It does not query iCloud or check whether an original exists there. Originals that are not stored locally are excluded. Files carrying the macOS dataless flag are not read.
- Files are saved with their internal library names, which are often UUIDs. Embedded EXIF and other data remain byte-for-byte intact, but Photos albums, people, favorites, edits, and user-facing original filenames are not exported.
- The image and video components of a Live Photo are uploaded as separate files. RAW and JPEG pairs are also separate. Google Drive will not reconstruct them as Apple Live Photos.
- The CLI does not inspect the Photos database for deleted or hidden status. If an original from Recently Deleted still exists under `originals/`, it may be included. Referenced originals stored outside the library are excluded.
- This tool therefore does not guarantee a complete backup of everything visible in the Photos app. Do not delete an entire Photos library based only on a successful run of this tool.
- Older libraries without an `originals/` directory return an explicit error. Quit Photos and avoid reorganizing or moving the library while the CLI is running.

## Requirements

- macOS and Go 1.24 or later
- [rclone](https://rclone.org/install/) with a configured Google Drive remote
- Read access to the Photos library. If you see `Operation not permitted`, open **System Settings → Privacy & Security → Full Disk Access**, allow the Terminal or other app running the command, and restart that app.

```sh
# When using Homebrew
brew install rclone
rclone config
```

If Homebrew is not installed, use rclone's official precompiled binary without `sudo`. The following commands install the Apple Silicon build in `~/.local/bin`:

```sh
mkdir -p "$HOME/.local/bin"
cd "$HOME/Downloads"
curl -O https://downloads.rclone.org/rclone-current-osx-arm64.zip
unzip -a rclone-current-osx-arm64.zip
cp rclone-*-osx-arm64/rclone "$HOME/.local/bin/rclone"
chmod 755 "$HOME/.local/bin/rclone"
"$HOME/.local/bin/rclone" version
"$HOME/.local/bin/rclone" config
```

For an Intel Mac, replace both occurrences of `arm64` with `amd64`. Check the CPU type with `uname -m`: `arm64` means Apple Silicon and `x86_64` means Intel. The [official rclone installation guide](https://rclone.org/install/) also provides browser-download and system-wide installation options.

In `rclone config`, create a remote named `gdrive`, choose `drive` as its storage type, and complete Google authorization in the browser. Select Google Drive, not Google Photos. rclone stores the OAuth token in its own configuration; this CLI does not store it in its state file.

When rclone is installed in `~/.local/bin` and that directory is not on `PATH`, pass its location to the CLI:

```sh
./photos-to-drive \
  --rclone "$HOME/.local/bin/rclone" \
  --library "$HOME/Pictures/Photos Library.photoslibrary" \
  --remote 'gdrive:MacPhotos' \
  --execute --limit 3
```

## Build and run

```sh
cd ~/work/go/photos-to-drive
go build -o photos-to-drive .

# Preview. Confirm the library name in Finder; you can also drag it into Terminal.
./photos-to-drive --library "$HOME/Pictures/Photos Library.photoslibrary"

# Start with three files
./photos-to-drive \
  --library "$HOME/Pictures/Photos Library.photoslibrary" \
  --remote 'gdrive:MacPhotos' \
  --execute --limit 3

# Continue using the same arguments
./photos-to-drive \
  --library "$HOME/Pictures/Photos Library.photoslibrary" \
  --remote 'gdrive:MacPhotos' \
  --execute
```

Photos library names may differ by language, and macOS may encode accented characters in a different Unicode form. If the path is not found, drag the actual library from Finder into Terminal to enter its path.

- Without `--execute`, the command only lists eligible files. It does not authenticate, upload, or create temporary copies.
- By default, state and temporary copies are kept in `.photos-to-drive/`. Use `--work /path/to/work` to choose another location. The work directory cannot be inside the Photos library.
- At most one file is staged at a time. A file is skipped, and the command exits with status 1, if staging it would leave less than 2 GiB free. Change the threshold with `--reserve-bytes`.
- `--limit` sets the maximum number of new files attempted in a run. Images and videos each count as one file.
- Press Ctrl+C to stop. A temporary copy interrupted during local copying is removed. A copy is retained when upload or verification fails. On the next run, the CLI removes leftovers from its dedicated staging directory and retries from the original.
- The destination path is `remote/library-path-identifier/original-subdirectory/MD5-internal-filename`. Changed content receives a new name, preserving the previous object. `rclone copyto --immutable` prevents overwriting an existing object with different content.
- After upload, the CLI reads the Google Drive object's size and MD5. It records progress atomically and removes the temporary copy only when both match. A missing checksum is treated as a failure.
- When resumed with the same source, destination, and work directory, verified files whose size and modification time have not changed are skipped. **Skipped cloud objects are not verified again.** If an object was manually removed from Drive, or you want a fresh verification, use a new `--work` directory.
- Resume works at file granularity, not from a byte offset within an interrupted upload.
- Keep `state.json`; it records the local path, destination, verification time, and checksum. Use a separate work directory for each source library or destination.
- A lock prevents concurrent use of the same work directory. Do not run separate work directories concurrently against the same destination.

## Test

```sh
go test -race ./...
go vet ./...
```

Tests use a local mock Photos library and transfer backend. They cover resume after success, verification failure, preservation of originals, free-space limits, destination mismatch, and cancellation. They do not access a real Google account.

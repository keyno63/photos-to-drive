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
```

For an Intel Mac, replace both occurrences of `arm64` with `amd64`. Check the CPU type with `uname -m`: `arm64` means Apple Silicon and `x86_64` means Intel. The [official rclone installation guide](https://rclone.org/install/) also provides browser-download and system-wide installation options.

## Configure Google Drive

rclone's shared Google OAuth client is being retired during 2026. Create your own OAuth client before configuring the remote. The complete upstream procedure is in [rclone: Making your own client ID](https://rclone.org/drive/#making-your-own-client-id).

In Google Cloud Console:

1. Create or select a project and enable **Google Drive API**.
2. Open **Google Auth Platform** and configure an External audience. While the app is in Testing, add the Google account that owns the destination Drive as a test user. Testing authorizations expire after seven days.
3. Under Data Access, add `https://www.googleapis.com/auth/drive.file`. This narrow, non-sensitive scope lets rclone access files it creates without granting access to every existing Drive file. Let rclone create the destination folder; a folder created manually in the Drive website might not be visible with this scope. See [Google's Drive scope guide](https://developers.google.com/workspace/drive/api/guides/api-specific-auth).
4. Create an OAuth client with application type **Desktop app**. Copy its Client ID and Client Secret. Do not publish either value or commit them to the repository.

Then configure rclone. If it is installed in `~/.local/bin`, use the full path shown here; otherwise `rclone config` is sufficient.

```sh
"$HOME/.local/bin/rclone" config
```

Use these answers. Menu numbers can change between rclone versions, so enter the text value where shown.

| Prompt | Answer |
| --- | --- |
| Create a new remote | `n` |
| `name>` | `gdrive` |
| `Storage>` | `drive` (Google Drive, not Google Cloud Storage or Google Photos) |
| `client_id>` | Your Desktop app Client ID |
| `client_secret>` | Your Desktop app Client Secret |
| `scope>` | `drive.file` or its displayed menu choice |
| `service_account_file>` | Leave blank |
| Edit advanced config? | `n` |
| Use web browser to authenticate? | `y` |
| Configure as a Shared Drive? | `n`, unless you specifically use a Google Workspace Shared Drive |
| Keep this remote? | `y` |

In the browser, sign in with the exact Google account added as a test user and grant access. An **Access blocked** page usually means that account is not listed under **Google Auth Platform → Audience → Test users**, or that the OAuth Client ID belongs to a different Cloud project.

Verify the saved remote and authentication before running this CLI:

```sh
"$HOME/.local/bin/rclone" listremotes
"$HOME/.local/bin/rclone" lsd "gdrive:"
"$HOME/.local/bin/rclone" about "gdrive:"
```

`listremotes` must include `gdrive:`. `lsd` may print nothing when the remote has not created any folders yet, but it must exit without an authentication error. `about` should show the Drive quota.

rclone stores the Client ID and OAuth tokens in `~/.config/rclone/rclone.conf`; this CLI does not copy them into `state.json`. Treat `rclone.conf` as a secret and do not commit or share it. If you see `didn't find section in config file ("gdrive")`, the configuration was not completed or the remote was saved under another name.

When rclone is installed in `~/.local/bin` and that directory is not on `PATH`, pass its location to the CLI:

```sh
./photos-to-drive \
  --rclone "$HOME/.local/bin/rclone" \
  --library "$HOME/Pictures/Photos Library.photoslibrary" \
  --remote 'gdrive:MacPhotos' \
  --execute --limit 3
```

## Check transfer status

The checkpoint records each file only after Google Drive reports the same size and MD5. View a summary without contacting or modifying Google Drive:

```sh
go run . \
  --library "$HOME/Pictures/Photos Library.photoslibrary" \
  --status
```

Write a detailed CSV containing every eligible local path and its status:

```sh
go run . \
  --library "$HOME/Pictures/Photos Library.photoslibrary" \
  --status \
  --report transfer-status.csv
```

The status values are:

- `verified`: size and modification time still match a successful upload-and-MD5-verification record.
- `pending`: no successful transfer record exists.
- `changed_since_verification`: the local file changed after its recorded transfer and must be processed again.
- `source_missing_after_verification`: a recorded source path is no longer in the current local inventory.

This status is based on the local checkpoint. It does not re-query objects that may have been manually removed from Google Drive later. For a final check before removing the local library, rerun the transfer with a new work directory. Existing matching Drive objects are retained and verified; missing objects are uploaded again.

```sh
go run . \
  --library "$HOME/Pictures/Photos Library.photoslibrary" \
  --remote 'gdrive:MacPhotos' \
  --work .photos-to-drive-final-audit \
  --execute
```

To map internal UUID paths back to items visible in Photos, including each item's original filename and title, create a Photos metadata report:

```sh
go run . \
  --library "$HOME/Pictures/Photos Library.photoslibrary" \
  --photos-report photos-transfer-status.csv
```

Photos opens while the metadata is read. On first use, macOS may ask for permission for Terminal to control Photos; allow it under **System Settings → Privacy & Security → Automation**. The report groups a Live Photo's image and video under one Photos item and marks the item `verified` only when every corresponding local resource is verified. Other possible values include `partially_verified`, `pending`, `changed_since_verification`, and `not_found_in_photos`.

The mapping uses the asset UUID shared by the Photos local identifier and the filename under `originals/`. Review any `not_found_in_photos` or `unmatched_local_filename` rows rather than assuming they are safe to remove.

Do not delete individual files inside `Photos Library.photoslibrary/originals`; that can corrupt the Photos library. This CLI intentionally does not currently delete Photos items. Deleting through Apple's supported Photos API moves the asset to Recently Deleted, but with iCloud Photos enabled that deletion also propagates to iCloud and other synced devices. Use the report to confirm completion, then choose a separate cleanup procedure that matches the intended iCloud behavior.

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

# Skip duplicate checking (normally leave this enabled)
./photos-to-drive \
  --library "$HOME/Pictures/Photos Library.photoslibrary" \
  --remote 'gdrive:MacPhotos' \
  --execute --skip-remote-check
```

Photos library names may differ by language, and macOS may encode accented characters in a different Unicode form. If the path is not found, drag the actual library from Finder into Terminal to enter its path.

- Without `--execute`, the command only lists eligible files. It does not authenticate, upload, or create temporary copies.
- By default, state and temporary copies are kept in `.photos-to-drive/`. Use `--work /path/to/work` to choose another location. The work directory cannot be inside the Photos library.
- At most one file is staged at a time. If free space falls to 2 GiB or less, processing stops safely; if an individual file does not fit above that reserve, only that file is skipped. `STOP` and `SKIP` messages show the file size, current free space, safety reserve, and usable staging space so the cause is explicit. The command exits with status 1 in either case. Free some space and rerun the same command to continue from the checkpoint. Change the threshold with `--reserve-bytes`, but lowering it on an almost-full system is not recommended.
- `--limit` sets the maximum number of new files attempted in a run. Images and videos each count as one file.
- By default, `--execute` recursively lists every file below `--remote` at startup. When an existing object has the same size and MD5 as a local original, the CLI records that Drive path as verified without uploading again, even when its name or subdirectory differs. Identical content uploaded earlier in the same run is also reused. Photos and videos use the same check. EXIF and capture timestamps are not used because they cannot prove file identity.
- `--skip-remote-check` skips the initial recursive Drive listing and duplicate comparison. Use it only when the destination is very large and you know it contains no duplicates.
- With the Google OAuth `drive.file` scope, only files created by rclone are visible to the listing. The remote needs broader listing access to inspect files added through the Drive website or another application. Items without an MD5 are excluded from matching and reported in the startup counts.
- At startup, the CLI prints the verified count, total eligible file count, remaining count, and start time. Each file line includes `[current position/total]`, the cumulative verified count, and elapsed time; the completion line includes total elapsed time. On resumed runs, files verified by earlier runs are included in the cumulative count.
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

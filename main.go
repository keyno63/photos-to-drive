// photos-to-drive copies locally present Photos originals, never changing the library.
package main

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type item struct {
	Path     string
	Size     int64
	Modified int64
}
type record struct {
	Size        int64
	Modified    int64
	MD5         string
	Destination string
	VerifiedAt  time.Time
}
type state struct {
	Source string
	Remote string
	Done   map[string]record
}
type options struct {
	library, remote, work, rclone, report, photosReport string
	execute                                             bool
	status                                              bool
	limit                                               int
	reserve                                             uint64
}
type runner func(context.Context, ...string) ([]byte, error)

func main() {
	var o options
	flag.StringVar(&o.library, "library", "", "path to .photoslibrary")
	flag.StringVar(&o.remote, "remote", "", "rclone destination, e.g. gdrive:MacPhotos")
	flag.StringVar(&o.work, "work", ".photos-to-drive", "private staging and state directory")
	flag.StringVar(&o.rclone, "rclone", "rclone", "rclone executable")
	flag.StringVar(&o.report, "report", "", "write local transfer status as CSV")
	flag.StringVar(&o.photosReport, "photos-report", "", "write Photos titles and transfer status as CSV")
	flag.BoolVar(&o.execute, "execute", false, "upload files (default: list only)")
	flag.BoolVar(&o.status, "status", false, "summarize verified, pending, and changed local files")
	flag.IntVar(&o.limit, "limit", 0, "maximum new files per run; 0 means all")
	flag.Uint64Var(&o.reserve, "reserve-bytes", 2<<30, "minimum free disk space to preserve")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var err error
	if o.photosReport != "" {
		err = showPhotosReport(ctx, o, os.Stdout, nil)
	} else if o.status || o.report != "" {
		err = showStatus(o, os.Stdout)
	} else {
		err = run(ctx, o, nil, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

type statusRow struct {
	Status      string
	Path        string
	Size        int64
	Modified    int64
	MD5         string
	Destination string
	VerifiedAt  time.Time
}

func showStatus(o options, out io.Writer) error {
	if o.library == "" {
		return errors.New("--library is required; see README.md")
	}
	lib, err := filepath.EvalSymlinks(o.library)
	if err != nil {
		return err
	}
	lib, err = filepath.Abs(lib)
	if err != nil {
		return err
	}
	root := filepath.Join(lib, "originals")
	st, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("cannot read originals (check macOS Full Disk Access; unsupported libraries are not scanned): %w", err)
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("originals must be a real directory")
	}
	files, skipped, err := inventory(root)
	if err != nil {
		return err
	}
	work, err := filepath.Abs(o.work)
	if err != nil {
		return err
	}
	work, err = resolveFuture(work)
	if err != nil {
		return err
	}
	s := state{Source: root, Done: map[string]record{}}
	statePath := filepath.Join(work, "state.json")
	if b, e := os.ReadFile(statePath); e == nil {
		if e = json.Unmarshal(b, &s); e != nil {
			return fmt.Errorf("invalid state: %w", e)
		}
		if s.Source != root || s.Done == nil {
			return errors.New("state belongs to another source or is invalid; use the matching --work")
		}
	} else if !os.IsNotExist(e) {
		return e
	}

	rows := make([]statusRow, 0, len(files)+len(s.Done))
	current := make(map[string]bool, len(files))
	verified, pending, changed := 0, 0, 0
	for _, f := range files {
		current[f.Path] = true
		r, ok := s.Done[f.Path]
		row := statusRow{Status: "pending", Path: f.Path, Size: f.Size, Modified: f.Modified}
		switch {
		case !ok:
			pending++
		case r.Size == f.Size && r.Modified == f.Modified:
			verified++
			row.Status = "verified"
			row.MD5, row.Destination, row.VerifiedAt = r.MD5, r.Destination, r.VerifiedAt
		default:
			changed++
			row.Status = "changed_since_verification"
			row.MD5, row.Destination, row.VerifiedAt = r.MD5, r.Destination, r.VerifiedAt
		}
		rows = append(rows, row)
	}
	missingKeys := make([]string, 0)
	for path := range s.Done {
		if !current[path] {
			missingKeys = append(missingKeys, path)
		}
	}
	sort.Strings(missingKeys)
	for _, path := range missingKeys {
		r := s.Done[path]
		rows = append(rows, statusRow{Status: "source_missing_after_verification", Path: path, Size: r.Size, Modified: r.Modified, MD5: r.MD5, Destination: r.Destination, VerifiedAt: r.VerifiedAt})
	}

	fmt.Fprintf(out, "Source: %s\n", root)
	if s.Remote != "" {
		fmt.Fprintf(out, "Recorded remote: %s\n", s.Remote)
	}
	fmt.Fprintf(out, "Eligible local files: %d; verified unchanged: %d; pending: %d; changed since verification: %d; source missing after verification: %d; skipped non-eligible entries: %d\n", len(files), verified, pending, changed, len(missingKeys), skipped)
	if pending == 0 && changed == 0 {
		fmt.Fprintln(out, "All currently eligible local files have a matching successful transfer record.")
	} else {
		fmt.Fprintln(out, "Not ready for whole-library removal: pending or changed files remain.")
	}
	if o.report != "" {
		if err = writeStatusCSV(o.report, lib, rows); err != nil {
			return err
		}
		fmt.Fprintf(out, "CSV report: %s\n", o.report)
	}
	fmt.Fprintln(out, "This is a local checkpoint report; it does not re-check files currently stored in Google Drive.")
	return nil
}

func writeStatusCSV(path, library string, rows []statusRow) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	abs, err = resolveFuture(abs)
	if err != nil {
		return err
	}
	if inside(library, abs) {
		return errors.New("--report must not be inside the Photos library")
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), "photos-to-drive-report-*.csv")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	w := csv.NewWriter(tmp)
	if err = w.Write([]string{"status", "source_path", "size_bytes", "modified_unix_nano", "md5_at_verification", "drive_destination", "verified_at"}); err == nil {
		for _, row := range rows {
			verifiedAt := ""
			if !row.VerifiedAt.IsZero() {
				verifiedAt = row.VerifiedAt.Format(time.RFC3339Nano)
			}
			if err = w.Write([]string{row.Status, row.Path, strconv.FormatInt(row.Size, 10), strconv.FormatInt(row.Modified, 10), row.MD5, row.Destination, verifiedAt}); err != nil {
				break
			}
		}
	}
	w.Flush()
	if err == nil {
		err = w.Error()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, abs)
}

func inside(parent, child string) bool {
	r, e := filepath.Rel(parent, child)
	return e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(os.PathSeparator))
}
func run(ctx context.Context, o options, invoke runner, out io.Writer) error {
	started := time.Now()
	if o.library == "" {
		return errors.New("--library is required; see README.md")
	}
	if o.limit < 0 {
		return errors.New("--limit must be nonnegative")
	}
	lib, err := filepath.EvalSymlinks(o.library)
	if err != nil {
		return err
	}
	lib, err = filepath.Abs(lib)
	if err != nil {
		return err
	}
	root := filepath.Join(lib, "originals")
	st, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("cannot read originals (check macOS Full Disk Access; unsupported libraries are not scanned): %w", err)
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("originals must be a real directory")
	}
	files, skipped, err := inventory(root)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Local files: %d; skipped entries: %d\n", len(files), skipped)
	if !o.execute {
		for _, f := range files {
			fmt.Fprintf(out, "%12d  %s\n", f.Size, f.Path)
		}
		fmt.Fprintln(out, "Preview only. No file contents read, no upload or deletion.")
		return nil
	}
	colon := strings.IndexByte(o.remote, ':')
	if colon <= 0 || strings.HasPrefix(o.remote, "/") || strings.HasPrefix(o.remote, ":") {
		return errors.New("--remote must name a configured rclone remote, e.g. gdrive:MacPhotos")
	}
	o.remote = strings.TrimRight(o.remote, "/")
	work, err := filepath.Abs(o.work)
	if err != nil {
		return err
	}
	// Resolve existing ancestors before creating any files, including work paths through symlinks.
	work, err = resolveFuture(work)
	if err != nil {
		return err
	}
	if inside(lib, work) || inside(work, lib) {
		return errors.New("--work and photo library must not contain each other")
	}
	if err = os.MkdirAll(work, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(work, "run.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another process is using this work directory")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	statePath := filepath.Join(work, "state.json")
	s := state{Source: root, Remote: o.remote, Done: map[string]record{}}
	if b, e := os.ReadFile(statePath); e == nil {
		if e = json.Unmarshal(b, &s); e != nil {
			return fmt.Errorf("invalid state: %w", e)
		}
		if s.Source != root || s.Remote != o.remote || s.Done == nil {
			return errors.New("state belongs to another source/destination or is invalid; use another --work")
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	verified := 0
	for _, f := range files {
		if r, ok := s.Done[f.Path]; ok && r.Size == f.Size && r.Modified == f.Modified {
			verified++
		}
	}
	fmt.Fprintf(out, "Started: %s\n", started.Format(time.RFC3339))
	printProgress(out, verified, len(files), started)
	if invoke == nil {
		if _, err = exec.LookPath(o.rclone); err != nil {
			return fmt.Errorf("install rclone and run rclone config first: %w", err)
		}
		invoke = func(ctx context.Context, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, o.rclone, args...)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			b, e := cmd.Output()
			if e != nil {
				return nil, fmt.Errorf("rclone %s: %w: %s", args[0], e, stderr.String())
			}
			return b, nil
		}
	}
	stage := filepath.Join(work, "staging")
	if st, e := os.Lstat(stage); e == nil && (!st.IsDir() || st.Mode()&os.ModeSymlink != 0) {
		return errors.New("staging must be a real directory")
	}
	if err = os.MkdirAll(stage, 0700); err != nil {
		return err
	}
	// This directory is exclusively owned by the CLI. Interrupted copies are retried from source.
	stale, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, f := range stale {
		if strings.HasPrefix(f.Name(), "export-") && !f.IsDir() {
			if err = os.Remove(filepath.Join(stage, f.Name())); err != nil {
				return err
			}
		}
	}
	completed, failed := 0, 0
	for i, f := range files {
		if err = ctx.Err(); err != nil {
			return err
		}
		if r, ok := s.Done[f.Path]; ok && r.Size == f.Size && r.Modified == f.Modified {
			continue
		}
		if o.limit > 0 && completed+failed >= o.limit {
			break
		}
		free, e := freeBytes(stage)
		if e != nil {
			return e
		}
		if free <= o.reserve {
			fmt.Fprintf(out, "[%d/%d] STOP free space is at or below the safety reserve before %s: free=%s, reserve=%s; elapsed=%s\n", i+1, len(files), f.Path, formatBytes(free), formatBytes(o.reserve), elapsed(started))
			failed++
			break
		}
		if uint64(f.Size) > free-o.reserve {
			fmt.Fprintf(out, "[%d/%d] SKIP file is too large for the available staging space: %s (file=%s, free=%s, reserve=%s, usable=%s); elapsed=%s\n", i+1, len(files), f.Path, formatBytes(uint64(f.Size)), formatBytes(free), formatBytes(o.reserve), formatBytes(free-o.reserve), elapsed(started))
			failed++
			continue
		}
		tmp, hash, e := exportFile(ctx, root, f, stage)
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(out, "[%d/%d] SKIP export %s: %v; elapsed=%s\n", i+1, len(files), f.Path, e, elapsed(started))
			failed++
			continue
		}
		// Content-addressed object names prevent overwriting an older or unrelated version.
		id := sha256.Sum256([]byte(root))
		dest := o.remote + "/" + hex.EncodeToString(id[:8]) + "/" + filepath.ToSlash(filepath.Dir(f.Path)) + "/" + hash + "-" + filepath.Base(f.Path)
		fmt.Fprintf(out, "[%d/%d] UPLOAD %s (%d bytes); verified=%d/%d; elapsed=%s\n", i+1, len(files), f.Path, f.Size, verified, len(files), elapsed(started))
		_, e = invoke(ctx, "copyto", tmp, dest, "--checksum", "--immutable", "--retries", "3", "--low-level-retries", "3", "--transfers", "1", "--checkers", "1")
		if e == nil {
			var b []byte
			b, e = invoke(ctx, "lsjson", dest, "--stat", "--hash", "--hash-type", "MD5")
			if e == nil {
				e = verify(b, f.Size, hash)
			}
		}
		if e != nil {
			return fmt.Errorf("transfer not verified; staged copy retained at %s: %w", tmp, e)
		}
		s.Done[f.Path] = record{f.Size, f.Modified, hash, dest, time.Now().UTC()}
		if e = saveState(statePath, s); e != nil {
			return e
		}
		if e = os.Remove(tmp); e != nil {
			return e
		}
		completed++
		verified++
		fmt.Fprintf(out, "[%d/%d] VERIFIED %s; verified=%d/%d; remaining=%d; elapsed=%s\n", i+1, len(files), f.Path, verified, len(files), len(files)-verified, elapsed(started))
	}
	verifiedNow := 0
	for _, f := range files {
		if r, ok := s.Done[f.Path]; ok && r.Size == f.Size && r.Modified == f.Modified {
			verifiedNow++
		}
	}
	fmt.Fprintf(out, "Checkpoint: %d/%d currently eligible files verified unchanged; %d remaining.\n", verifiedNow, len(files), len(files)-verifiedNow)
	fmt.Fprintf(out, "Complete: %d newly verified; %d skipped/failed; elapsed=%s. Photos originals were not modified.\n", completed, failed, elapsed(started))
	if failed > 0 {
		return errors.New("some files were not transferred; see SKIP messages and rerun to retry")
	}
	return nil
}

func printProgress(out io.Writer, verified, total int, started time.Time) {
	percent := 100.0
	if total > 0 {
		percent = float64(verified) * 100 / float64(total)
	}
	fmt.Fprintf(out, "Progress: %d/%d verified (%.1f%%); %d remaining; elapsed=%s\n", verified, total, percent, total-verified, elapsed(started))
}

func elapsed(started time.Time) time.Duration {
	d := time.Since(started).Truncate(time.Second)
	if d < 0 {
		return 0
	}
	return d
}

func resolveFuture(p string) (string, error) {
	if _, e := os.Lstat(p); e == nil {
		return filepath.EvalSymlinks(p)
	} else if !os.IsNotExist(e) {
		return "", e
	}
	parent := filepath.Dir(p)
	if parent == p {
		return "", errors.New("cannot resolve work directory")
	}
	r, e := resolveFuture(parent)
	return filepath.Join(r, filepath.Base(p)), e
}

func inventory(root string) ([]item, int, error) {
	var files []item
	skipped := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			skipped++
			return nil
		}
		s, e := d.Info()
		if e != nil {
			return e
		}
		if !s.Mode().IsRegular() || s.Size() == 0 || !media(p) || dataless(s) {
			skipped++
			return nil
		}
		r, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		files = append(files, item{r, s.Size(), s.ModTime().UnixNano()})
		return nil
	})
	return files, skipped, err
}
func media(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jpg", ".jpeg", ".heic", ".heif", ".png", ".gif", ".tif", ".tiff", ".webp", ".avif", ".bmp", ".dng", ".raw", ".cr2", ".cr3", ".nef", ".nrw", ".arw", ".raf", ".orf", ".rw2", ".pef", ".srw", ".mov", ".mp4", ".m4v", ".avi", ".3gp", ".mts", ".m2ts":
		return true
	}
	return false
}
func exportFile(ctx context.Context, root string, f item, stage string) (name, hash string, err error) {
	p := filepath.Join(root, f.Path)
	resolved, e := filepath.EvalSymlinks(p)
	if e != nil {
		return "", "", e
	}
	if resolved != p || !inside(root, resolved) {
		return "", "", errors.New("symlink or escaped source")
	}
	fd, e := syscall.Open(p, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return "", "", e
	}
	src := os.NewFile(uintptr(fd), p)
	defer src.Close()
	before, e := src.Stat()
	if e != nil {
		return "", "", e
	}
	if !before.Mode().IsRegular() || dataless(before) || before.Size() != f.Size || before.ModTime().UnixNano() != f.Modified {
		return "", "", errors.New("source changed or is not local")
	}
	dst, e := os.CreateTemp(stage, "export-*"+filepath.Ext(f.Path))
	if e != nil {
		return "", "", e
	}
	name = dst.Name()
	defer func() {
		dst.Close()
		if err != nil {
			os.Remove(name)
		}
	}()
	h := md5.New()
	buf := make([]byte, 1<<20)
	var total int64
	for {
		if e = ctx.Err(); e != nil {
			return name, "", e
		}
		n, re := src.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > f.Size {
				return name, "", errors.New("source grew during export")
			}
			if _, e = io.MultiWriter(dst, h).Write(buf[:n]); e != nil {
				return name, "", e
			}
		}
		if re == io.EOF {
			break
		}
		if re != nil {
			return name, "", re
		}
	}
	after, e := src.Stat()
	if e != nil {
		return name, "", e
	}
	if total != f.Size || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return name, "", errors.New("source changed during export")
	}
	if e = dst.Sync(); e != nil {
		return name, "", e
	}
	if e = dst.Close(); e != nil {
		return name, "", e
	}
	return name, hex.EncodeToString(h.Sum(nil)), nil
}

func formatBytes(n uint64) string {
	const unit = uint64(1024)
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	for _, name := range units {
		value /= float64(unit)
		if value < float64(unit) || name == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", value, name)
		}
	}
	return fmt.Sprintf("%d B", n)
}

func verify(b []byte, size int64, hash string) error {
	var v struct {
		Size   int64
		IsDir  bool
		Hashes map[string]string
	}
	if e := json.Unmarshal(b, &v); e != nil {
		return e
	}
	remoteMD5 := ""
	for name, value := range v.Hashes {
		if strings.EqualFold(name, "MD5") {
			remoteMD5 = value
			break
		}
	}
	if v.IsDir || v.Size != size || hash == "" || !strings.EqualFold(remoteMD5, hash) {
		return errors.New("remote size/MD5 mismatch or unavailable checksum")
	}
	return nil
}
func saveState(p string, s state) error {
	b, e := json.MarshalIndent(s, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(p), "state-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), p)
}

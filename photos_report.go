package main

import (
	"context"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

//go:embed photos_metadata.applescript
var photosMetadataAppleScript string

type photoMetadata struct {
	ID       string
	Filename string
	Title    string
}

type photoMetadataReader func(context.Context) ([]photoMetadata, error)

type photoReportRow struct {
	UUID              string
	PhotosID          string
	Filename          string
	Title             string
	Status            string
	VerifiedFiles     int
	LocalFiles        int
	SourcePaths       string
	DriveDestinations string
}

func showPhotosReport(ctx context.Context, o options, out io.Writer, read photoMetadataReader) error {
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
	if b, readErr := os.ReadFile(filepath.Join(work, "state.json")); readErr == nil {
		if err = json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("invalid state: %w", err)
		}
		if s.Source != root || s.Done == nil {
			return errors.New("state belongs to another source or is invalid; use the matching --work")
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}

	groups := make(map[string][]item)
	unmatchedFiles := make([]item, 0)
	for _, f := range files {
		uuid, ok := assetUUID(f.Path)
		if !ok {
			unmatchedFiles = append(unmatchedFiles, f)
			continue
		}
		groups[uuid] = append(groups[uuid], f)
	}
	if read == nil {
		read = readPhotosMetadata
	}
	photosItems, err := read(ctx)
	if err != nil {
		return fmt.Errorf("read Photos metadata: %w", err)
	}
	rows := make([]photoReportRow, 0, len(groups)+len(unmatchedFiles))
	seen := make(map[string]bool)
	verifiedAssets, pendingAssets, partialAssets, changedAssets := 0, 0, 0, 0
	for _, p := range photosItems {
		uuid, ok := photosAssetUUID(p.ID)
		if !ok {
			continue
		}
		assetFiles, exists := groups[uuid]
		if !exists {
			continue
		}
		seen[uuid] = true
		row := buildPhotoRow(uuid, p, assetFiles, s)
		switch row.Status {
		case "verified":
			verifiedAssets++
		case "partially_verified":
			partialAssets++
		case "changed_since_verification":
			changedAssets++
		default:
			pendingAssets++
		}
		rows = append(rows, row)
	}
	missingUUIDs := make([]string, 0)
	for uuid := range groups {
		if !seen[uuid] {
			missingUUIDs = append(missingUUIDs, uuid)
		}
	}
	sort.Strings(missingUUIDs)
	for _, uuid := range missingUUIDs {
		row := buildPhotoRow(uuid, photoMetadata{}, groups[uuid], s)
		row.Status = "not_found_in_photos"
		rows = append(rows, row)
	}
	for _, f := range unmatchedFiles {
		rows = append(rows, photoReportRow{Status: "unmatched_local_filename", LocalFiles: 1, SourcePaths: f.Path})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Filename == rows[j].Filename {
			return rows[i].UUID < rows[j].UUID
		}
		return rows[i].Filename < rows[j].Filename
	})
	if err = writePhotoReportCSV(o.photosReport, lib, rows); err != nil {
		return err
	}
	fmt.Fprintf(out, "Photos items matched: %d; verified: %d; partially verified: %d; pending: %d; changed: %d; not found in Photos: %d; local names without an asset UUID: %d; skipped non-eligible entries: %d\n", verifiedAssets+partialAssets+pendingAssets+changedAssets, verifiedAssets, partialAssets, pendingAssets, changedAssets, len(missingUUIDs), len(unmatchedFiles), skipped)
	fmt.Fprintf(out, "Photos metadata report: %s\n", o.photosReport)
	fmt.Fprintln(out, "No Photos items were changed or deleted.")
	return nil
}

func buildPhotoRow(uuid string, p photoMetadata, files []item, s state) photoReportRow {
	row := photoReportRow{UUID: uuid, PhotosID: p.ID, Filename: p.Filename, Title: p.Title, Status: "pending", LocalFiles: len(files)}
	paths := make([]string, 0, len(files))
	destinations := make([]string, 0, len(files))
	verified, changed := 0, false
	for _, f := range files {
		paths = append(paths, f.Path)
		if r, ok := s.Done[f.Path]; ok {
			if r.Size == f.Size && r.Modified == f.Modified {
				verified++
				destinations = append(destinations, r.Destination)
			} else {
				changed = true
			}
		}
	}
	sort.Strings(paths)
	sort.Strings(destinations)
	row.VerifiedFiles = verified
	row.SourcePaths = strings.Join(paths, ";")
	row.DriveDestinations = strings.Join(destinations, ";")
	switch {
	case changed:
		row.Status = "changed_since_verification"
	case verified == len(files) && len(files) > 0:
		row.Status = "verified"
	case verified > 0:
		row.Status = "partially_verified"
	}
	return row
}

func assetUUID(path string) (string, bool) {
	base := strings.ToUpper(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	if len(base) < 36 {
		return "", false
	}
	base = base[:36]
	for i, c := range base {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return "", false
			}
			continue
		}
		if !strings.ContainsRune("0123456789ABCDEF", c) {
			return "", false
		}
	}
	return base, true
}

func photosAssetUUID(id string) (string, bool) {
	first := strings.SplitN(id, "/", 2)[0]
	return assetUUID(first + ".asset")
}

func readPhotosMetadata(ctx context.Context) ([]photoMetadata, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-")
	cmd.Stdin = strings.NewReader(photosMetadataAppleScript)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("osascript: %w: %s (allow Terminal to control Photos in System Settings > Privacy & Security > Automation)", err, strings.TrimSpace(stderr.String()))
	}
	items := make([]photoMetadata, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(strings.TrimSuffix(line, "\r"), "\t", 3)
		for len(parts) < 3 {
			parts = append(parts, "")
		}
		items = append(items, photoMetadata{ID: parts[0], Filename: parts[1], Title: parts[2]})
	}
	return items, nil
}

func writePhotoReportCSV(path, library string, rows []photoReportRow) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	abs, err = resolveFuture(abs)
	if err != nil {
		return err
	}
	if inside(library, abs) {
		return errors.New("--photos-report must not be inside the Photos library")
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), "photos-to-drive-photos-report-*.csv")
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
	err = w.Write([]string{"asset_uuid", "photos_id", "original_filename", "title", "transfer_status", "verified_resource_files", "local_resource_files", "source_paths", "drive_destinations"})
	for _, row := range rows {
		if err != nil {
			break
		}
		err = w.Write([]string{row.UUID, row.PhotosID, row.Filename, row.Title, row.Status, strconv.Itoa(row.VerifiedFiles), strconv.Itoa(row.LocalFiles), row.SourcePaths, row.DriveDestinations})
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

package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) (options, string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "test.photoslibrary")
	root := filepath.Join(lib, "originals", "A")
	if e := os.MkdirAll(root, 0700); e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(root, "a.jpg")
	if e := os.WriteFile(p, []byte("original-photo"), 0600); e != nil {
		t.Fatal(e)
	}
	return options{library: lib, remote: "gdrive:Archive", work: filepath.Join(dir, "work"), execute: true, skipRemoteCheck: true}, p
}
func backend(t *testing.T, bad bool, calls *int) runner {
	t.Helper()
	var data []byte
	return func(ctx context.Context, a ...string) ([]byte, error) {
		switch a[0] {
		case "copyto":
			*calls++
			var e error
			data, e = os.ReadFile(a[1])
			if e != nil {
				return nil, e
			}
			if !strings.Contains(strings.Join(a, " "), "--immutable") {
				t.Fatal("missing immutable")
			}
			return nil, nil
		case "lsjson":
			h := md5.Sum(data)
			sum := hex.EncodeToString(h[:])
			if bad {
				sum = "bad"
			}
			return json.Marshal(map[string]any{"Size": len(data), "Hashes": map[string]string{"MD5": sum}})
		}
		return nil, errors.New("unexpected command")
	}
}
func TestTransferResumeAndOriginalUnchanged(t *testing.T) {
	o, p := fixture(t)
	n := 0
	b := backend(t, false, &n)
	for i := 0; i < 2; i++ {
		if e := run(context.Background(), o, b, &bytes.Buffer{}); e != nil {
			t.Fatal(e)
		}
	}
	if n != 1 {
		t.Fatalf("copies=%d", n)
	}
	data, e := os.ReadFile(p)
	if e != nil || string(data) != "original-photo" {
		t.Fatal("source modified")
	}
	entries, e := os.ReadDir(filepath.Join(o.work, "staging"))
	if e != nil || len(entries) != 0 {
		t.Fatal("staging not cleaned")
	}
}

func TestRunShowsTotalCurrentAndElapsedTime(t *testing.T) {
	o, p := fixture(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(p), "b.jpg"), []byte("second-photo"), 0600); err != nil {
		t.Fatal(err)
	}
	n := 0
	var out bytes.Buffer
	if err := run(context.Background(), o, backend(t, false, &n), &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"Progress: 0/2 verified (0.0%); 2 remaining; elapsed=",
		"[1/2] UPLOAD A/a.jpg",
		"[1/2] VERIFIED A/a.jpg; verified=1/2; remaining=1; elapsed=",
		"[2/2] UPLOAD A/b.jpg",
		"[2/2] VERIFIED A/b.jpg; verified=2/2; remaining=0; elapsed=",
		"Complete: 2 newly verified; 0 skipped/failed; elapsed=",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in output:\n%s", want, text)
		}
	}
	out.Reset()
	if err := run(context.Background(), o, backend(t, false, &n), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Progress: 2/2 verified (100.0%); 0 remaining; elapsed=") {
		t.Fatalf("resume did not include existing checkpoint:\n%s", out.String())
	}
}

func TestRemoteDuplicateIsReusedWithoutUpload(t *testing.T) {
	o, _ := fixture(t)
	o.skipRemoteCheck = false
	h := md5.Sum([]byte("original-photo"))
	wantHash := hex.EncodeToString(h[:])
	copyCalls := 0
	invoke := func(ctx context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "mkdir":
			return nil, nil
		case "lsjson":
			return json.Marshal([]map[string]any{{
				"Path": "existing/renamed-photo.jpg", "Size": len("original-photo"),
				"Hashes": map[string]string{"md5": strings.ToUpper(wantHash)},
			}})
		case "copyto":
			copyCalls++
			return nil, nil
		default:
			return nil, errors.New("unexpected command")
		}
	}
	var out bytes.Buffer
	if err := run(context.Background(), o, invoke, &out); err != nil {
		t.Fatal(err)
	}
	if copyCalls != 0 {
		t.Fatalf("uploaded an existing duplicate %d times", copyCalls)
	}
	if !strings.Contains(out.String(), "MATCHED A/a.jpg; existing=gdrive:Archive/existing/renamed-photo.jpg") ||
		!strings.Contains(out.String(), "Remote matches reused: 1; uploads completed: 0") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
	b, err := os.ReadFile(filepath.Join(o.work, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s state
	if err = json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if got := s.Done["A/a.jpg"].Destination; got != "gdrive:Archive/existing/renamed-photo.jpg" {
		t.Fatalf("destination=%q", got)
	}
}

func TestShowRemoteDuplicates(t *testing.T) {
	o := options{remote: "gdrive:photo_store"}
	hash := "0123456789abcdef0123456789abcdef"
	invoke := func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] != "lsjson" || args[1] != o.remote {
			t.Fatalf("args=%v", args)
		}
		return json.Marshal([]map[string]any{
			{"Path": "0/hoge1.png", "Size": 100, "Hashes": map[string]string{"MD5": hash}},
			{"Path": "2/hoge5.png", "Size": 100, "Hashes": map[string]string{"md5": strings.ToUpper(hash)}},
			{"Path": "unique.mov", "Size": 200, "Hashes": map[string]string{"MD5": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
			{"Path": "no-hash.mp4", "Size": 300},
		})
	}
	var out bytes.Buffer
	if err := showRemoteDuplicates(context.Background(), o, invoke, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"Remote files: 4; usable size/MD5 records: 3; without usable MD5: 1",
		"Duplicate groups: 1; files in duplicate groups: 2; reclaimable if one copy per group is kept: 100 B",
		"gdrive:photo_store/0/hoge1.png",
		"gdrive:photo_store/2/hoge5.png",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in output:\n%s", want, text)
		}
	}
}

func TestRemoteNonDuplicateIsUploaded(t *testing.T) {
	o, _ := fixture(t)
	o.skipRemoteCheck = false
	copyCalls := 0
	var uploaded []byte
	invoke := func(ctx context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "mkdir":
			return nil, nil
		case "copyto":
			copyCalls++
			var err error
			uploaded, err = os.ReadFile(args[1])
			return nil, err
		case "lsjson":
			if args[1] == o.remote {
				return json.Marshal([]map[string]any{{
					"Path": "existing/different.jpg", "Size": len("original-photo"),
					"Hashes": map[string]string{"MD5": "00000000000000000000000000000000"},
				}})
			}
			h := md5.Sum(uploaded)
			return json.Marshal(map[string]any{
				"Size": len(uploaded), "Hashes": map[string]string{"MD5": hex.EncodeToString(h[:])},
			})
		default:
			return nil, errors.New("unexpected command")
		}
	}
	var out bytes.Buffer
	if err := run(context.Background(), o, invoke, &out); err != nil {
		t.Fatal(err)
	}
	if copyCalls != 1 || !strings.Contains(out.String(), "Remote matches reused: 0; uploads completed: 1") {
		t.Fatalf("copyCalls=%d output:\n%s", copyCalls, out.String())
	}
}

func TestPreviewDoesNotContactRemoteByDefault(t *testing.T) {
	o, _ := fixture(t)
	o.execute = false
	o.skipRemoteCheck = false
	if err := run(context.Background(), o, func(context.Context, ...string) ([]byte, error) {
		t.Fatal("preview contacted remote")
		return nil, nil
	}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestStatusAndCSVReport(t *testing.T) {
	o, p := fixture(t)
	n := 0
	if err := run(context.Background(), o, backend(t, false, &n), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(filepath.Dir(p), "b.jpg")
	if err := os.WriteFile(second, []byte("pending-photo"), 0600); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(filepath.Dir(o.work), "status.csv")
	o.report = report
	var out bytes.Buffer
	if err := showStatus(o, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "verified unchanged: 1; pending: 1; changed since verification: 0") {
		t.Fatalf("unexpected status: %s", out.String())
	}
	b, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, "verified,A/a.jpg") || !strings.Contains(text, "pending,A/b.jpg") {
		t.Fatalf("unexpected report: %s", text)
	}
	if err = os.WriteFile(p, []byte("changed-photo"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	o.report = ""
	if err = showStatus(o, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "verified unchanged: 0; pending: 1; changed since verification: 1") {
		t.Fatalf("unexpected changed status: %s", out.String())
	}
}
func TestVerificationFailureRetainsStageWithoutCheckpoint(t *testing.T) {
	o, p := fixture(t)
	n := 0
	if e := run(context.Background(), o, backend(t, true, &n), &bytes.Buffer{}); e == nil {
		t.Fatal("expected mismatch")
	}
	entries, e := os.ReadDir(filepath.Join(o.work, "staging"))
	if e != nil || len(entries) != 1 {
		t.Fatal("staging lost")
	}
	if _, e = os.Stat(filepath.Join(o.work, "state.json")); !os.IsNotExist(e) {
		t.Fatal("unexpected checkpoint")
	}
	if _, e = os.Stat(p); e != nil {
		t.Fatal("source lost")
	}
	if e = run(context.Background(), o, backend(t, false, &n), &bytes.Buffer{}); e != nil {
		t.Fatal(e)
	}
	if n != 2 {
		t.Fatal(n)
	}
}
func TestDryRunCreatesNothing(t *testing.T) {
	o, _ := fixture(t)
	o.execute = false
	if e := run(context.Background(), o, func(context.Context, ...string) ([]byte, error) { t.Fatal("called remote"); return nil, nil }, &bytes.Buffer{}); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(o.work); !os.IsNotExist(e) {
		t.Fatal("created work")
	}
}
func TestSourceChangesAreRetried(t *testing.T) {
	o, p := fixture(t)
	n := 0
	b := backend(t, false, &n)
	if e := run(context.Background(), o, b, &bytes.Buffer{}); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte("changed-original-photo"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := run(context.Background(), o, b, &bytes.Buffer{}); e != nil {
		t.Fatal(e)
	}
	if n != 2 {
		t.Fatal(n)
	}
}
func TestLowSpaceSkipsWithoutUpload(t *testing.T) {
	o, _ := fixture(t)
	o.reserve = ^uint64(0)
	n := 0
	var out bytes.Buffer
	if e := run(context.Background(), o, backend(t, false, &n), &out); e == nil {
		t.Fatal("expected insufficient space")
	}
	if n != 0 {
		t.Fatal("uploaded despite reserve")
	}
	if strings.Count(out.String(), "STOP free space is at or below the safety reserve") != 1 || !strings.Contains(out.String(), "free=") || !strings.Contains(out.String(), "reserve=") {
		t.Fatalf("unexpected output: %s", out.String())
	}
}

func TestFormatBytes(t *testing.T) {
	tests := map[uint64]string{
		0:           "0 B",
		1023:        "1023 B",
		1024:        "1.0 KiB",
		1024 * 1024: "1.0 MiB",
		2 << 30:     "2.0 GiB",
	}
	for input, want := range tests {
		if got := formatBytes(input); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", input, got, want)
		}
	}
}
func TestStateDestinationMismatch(t *testing.T) {
	o, _ := fixture(t)
	n := 0
	b := backend(t, false, &n)
	if e := run(context.Background(), o, b, &bytes.Buffer{}); e != nil {
		t.Fatal(e)
	}
	o.remote = "gdrive:Other"
	if e := run(context.Background(), o, b, &bytes.Buffer{}); e == nil {
		t.Fatal("accepted mismatched state")
	}
}
func TestRejectWorkInsideLibrary(t *testing.T) {
	o, _ := fixture(t)
	o.work = filepath.Join(o.library, "work")
	if e := run(context.Background(), o, nil, &bytes.Buffer{}); e == nil {
		t.Fatal("accepted work inside library")
	}
	if _, e := os.Stat(o.work); !os.IsNotExist(e) {
		t.Fatal("wrote to library")
	}
}
func TestInventoryExcludesSymlinks(t *testing.T) {
	o, p := fixture(t)
	if e := os.Symlink(p, filepath.Join(filepath.Dir(p), "link.jpg")); e != nil {
		t.Fatal(e)
	}
	files, skip, e := inventory(filepath.Join(o.library, "originals"))
	if e != nil || len(files) != 1 || skip != 1 {
		t.Fatal(files, skip, e)
	}
}
func TestCancelStopsBeforeUpload(t *testing.T) {
	o, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := 0
	if e := run(ctx, o, backend(t, false, &n), &bytes.Buffer{}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if n != 0 {
		t.Fatal("uploaded")
	}
}
func TestVerifyRejectsMissingHashAndWrongSize(t *testing.T) {
	for _, s := range []string{`{"Size":3}`, `{"Size":4,"Hashes":{"MD5":"abc"}}`, `{"Size":3,"IsDir":true,"Hashes":{"MD5":"abc"}}`, `not-json`} {
		if verify([]byte(s), 3, "abc") == nil {
			t.Fatal(s)
		}
	}
}

func TestVerifyAcceptsCaseInsensitiveMD5Key(t *testing.T) {
	for _, s := range []string{
		`{"Size":3,"Hashes":{"MD5":"abc"}}`,
		`{"Size":3,"Hashes":{"md5":"abc"}}`,
		`{"Size":3,"Hashes":{"Md5":"ABC"}}`,
	} {
		if err := verify([]byte(s), 3, "abc"); err != nil {
			t.Fatalf("verify(%s): %v", s, err)
		}
	}
}

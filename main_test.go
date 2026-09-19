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
	return options{library: lib, remote: "gdrive:Archive", work: filepath.Join(dir, "work"), execute: true}, p
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
	if e := run(context.Background(), o, backend(t, false, &n), &bytes.Buffer{}); e == nil {
		t.Fatal("expected insufficient space")
	}
	if n != 0 {
		t.Fatal("uploaded despite reserve")
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

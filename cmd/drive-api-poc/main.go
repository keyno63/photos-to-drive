// drive-api-poc accesses Google Drive directly without rclone. Uploads require
// both --upload and --execute; otherwise upload requests are previewed only.
package main

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

type options struct {
	credentials string
	token       string
	folderID    string
	uploads     stringList
	execute     bool
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type remoteFile struct {
	Path string
	Size int64
	MD5  string
}

type contentKey struct {
	Size int64
	MD5  string
}

type duplicateGroup struct {
	Key   contentKey
	Paths []string
}

type preparedUpload struct {
	File     *os.File
	Path     string
	Name     string
	Size     int64
	Modified time.Time
	MD5      string
}

func main() {
	var o options
	flag.StringVar(&o.credentials, "credentials", "", "OAuth Desktop app client-secret JSON file")
	flag.StringVar(&o.token, "token", "", "OAuth token file (default: user config directory)")
	flag.StringVar(&o.folderID, "folder-id", "", "Google Drive folder ID to scan recursively")
	flag.Var(&o.uploads, "upload", "local file to upload; may be specified multiple times")
	flag.BoolVar(&o.execute, "execute", false, "perform uploads requested by --upload (default: preview only)")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := run(ctx, o, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options, out io.Writer) error {
	if o.credentials == "" {
		return errors.New("--credentials is required")
	}
	if o.folderID == "" {
		return errors.New("--folder-id is required")
	}
	if strings.ContainsAny(o.folderID, "'\r\n") {
		return errors.New("--folder-id contains invalid characters")
	}
	credentials, err := filepath.Abs(expandHome(o.credentials))
	if err != nil {
		return err
	}
	tokenPath, err := resolveTokenPath(o.token)
	if err != nil {
		return err
	}

	service, err := newDriveService(ctx, credentials, tokenPath, out)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Scanning Drive folder ID %s recursively...\n", o.folderID)
	files, err := listFolderTree(ctx, service, o.folderID, out)
	if err != nil {
		return err
	}
	if len(o.uploads) == 0 {
		printDuplicates(out, "drive-folder:"+o.folderID, files)
		return nil
	}
	if err = uploadFiles(ctx, service, o.folderID, o.uploads, files, o.execute, out); err != nil {
		return err
	}
	return nil
}

func uploadFiles(ctx context.Context, service *drive.Service, folderID string, paths []string, remoteFiles []remoteFile, execute bool, out io.Writer) error {
	index := make(map[contentKey]string, len(remoteFiles))
	for _, file := range remoteFiles {
		if file.MD5 == "" || file.Size < 0 {
			continue
		}
		key := contentKey{Size: file.Size, MD5: strings.ToLower(file.MD5)}
		if _, exists := index[key]; !exists {
			index[key] = file.Path
		}
	}

	started := time.Now()
	uploaded, matched, planned := 0, 0, 0
	for i, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		prepared, err := prepareUpload(path)
		if err != nil {
			return fmt.Errorf("prepare upload %s: %w", path, err)
		}
		key := contentKey{Size: prepared.Size, MD5: prepared.MD5}
		if existing, ok := index[key]; ok {
			prepared.File.Close()
			matched++
			fmt.Fprintf(out, "[%d/%d] MATCHED %s; existing=%s; elapsed=%s\n", i+1, len(paths), prepared.Path, existing, time.Since(started).Truncate(time.Second))
			continue
		}
		if !execute {
			prepared.File.Close()
			planned++
			index[key] = "planned:" + prepared.Name
			fmt.Fprintf(out, "[%d/%d] WOULD UPLOAD %s as %s (%s); elapsed=%s\n", i+1, len(paths), prepared.Path, prepared.Name, formatBytes(uint64(prepared.Size)), time.Since(started).Truncate(time.Second))
			continue
		}

		fmt.Fprintf(out, "[%d/%d] UPLOAD %s as %s (%s)\n", i+1, len(paths), prepared.Path, prepared.Name, formatBytes(uint64(prepared.Size)))
		created, err := service.Files.Create(&drive.File{
			Name:    prepared.Name,
			Parents: []string{folderID},
		}).
			Media(prepared.File, googleapi.ChunkSize(8<<20)).
			ProgressUpdater(func(current, total int64) {
				percent := 100.0
				if total > 0 {
					percent = float64(current) * 100 / float64(total)
				}
				fmt.Fprintf(out, "[%d/%d] UPLOADING %.1f%% (%s/%s); elapsed=%s\n", i+1, len(paths), percent, formatBytes(uint64(current)), formatBytes(uint64(total)), time.Since(started).Truncate(time.Second))
			}).
			Fields("id,name,size,md5Checksum,parents").
			SupportsAllDrives(true).
			Context(ctx).
			Do()
		closeErr := prepared.File.Close()
		if err != nil {
			return fmt.Errorf("upload %s: %w", prepared.Path, err)
		}
		if closeErr != nil {
			return closeErr
		}
		if err = sourceUnchanged(prepared); err != nil {
			return fmt.Errorf("uploaded Drive file ID %s but source validation failed: %w", created.Id, err)
		}
		verified, err := service.Files.Get(created.Id).
			Fields("id,name,size,md5Checksum,parents").
			SupportsAllDrives(true).
			Context(ctx).
			Do()
		if err != nil {
			return fmt.Errorf("verify uploaded Drive file ID %s: %w", created.Id, err)
		}
		if verified.Size != prepared.Size || !strings.EqualFold(verified.Md5Checksum, prepared.MD5) {
			return fmt.Errorf("uploaded Drive file ID %s failed size/MD5 verification", created.Id)
		}
		uploaded++
		index[key] = verified.Name
		fmt.Fprintf(out, "[%d/%d] VERIFIED id=%s name=%s MD5=%s; elapsed=%s\n", i+1, len(paths), verified.Id, verified.Name, verified.Md5Checksum, time.Since(started).Truncate(time.Second))
	}
	if !execute {
		fmt.Fprintf(out, "Dry run: %d would upload; %d existing or planned matches reused; elapsed=%s. Re-run with --execute to upload.\n", planned, matched, time.Since(started).Truncate(time.Second))
		return nil
	}
	fmt.Fprintf(out, "Complete: %d uploaded and verified; %d existing matches reused; elapsed=%s. Local files were not modified.\n", uploaded, matched, time.Since(started).Truncate(time.Second))
	return nil
}

func prepareUpload(path string) (*preparedUpload, error) {
	abs, err := filepath.Abs(expandHome(path))
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("source must be a regular non-symlink file")
	}
	file, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	h := md5.New()
	if _, err = io.Copy(h, file); err != nil {
		file.Close()
		return nil, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	hash := hex.EncodeToString(h.Sum(nil))
	return &preparedUpload{
		File:     file,
		Path:     abs,
		Name:     hash + "-" + filepath.Base(abs),
		Size:     info.Size(),
		Modified: info.ModTime(),
		MD5:      hash,
	}, nil
}

func sourceUnchanged(prepared *preparedUpload) error {
	info, err := os.Stat(prepared.Path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != prepared.Size || !info.ModTime().Equal(prepared.Modified) {
		return errors.New("local source changed during upload")
	}
	return nil
}

func newDriveService(ctx context.Context, credentialsPath, tokenPath string, out io.Writer) (*drive.Service, error) {
	b, err := os.ReadFile(credentialsPath)
	if err != nil {
		return nil, fmt.Errorf("read OAuth credentials: %w", err)
	}
	config, err := google.ConfigFromJSON(b, drive.DriveFileScope, drive.DriveMetadataReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("parse OAuth credentials: %w", err)
	}

	token, err := readToken(tokenPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if token == nil {
		token, err = authorizeDesktop(ctx, config, out)
		if err != nil {
			return nil, err
		}
		if err = writeToken(tokenPath, token); err != nil {
			return nil, err
		}
	}

	source := &savingTokenSource{
		source: oauth2.ReuseTokenSource(token, config.TokenSource(ctx, token)),
		path:   tokenPath,
		last:   token,
	}
	client := oauth2.NewClient(ctx, source)
	service, err := drive.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("create Drive service: %w", err)
	}
	return service, nil
}

type savingTokenSource struct {
	mu     sync.Mutex
	source oauth2.TokenSource
	path   string
	last   *oauth2.Token
}

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, err := s.source.Token()
	if err != nil {
		return nil, err
	}
	if !sameToken(s.last, token) {
		if err = writeToken(s.path, token); err != nil {
			return nil, err
		}
		s.last = token
	}
	return token, nil
}

func sameToken(a, b *oauth2.Token) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken && a.Expiry.Equal(b.Expiry)
}

func authorizeDesktop(ctx context.Context, config *oauth2.Config, out io.Writer) (*oauth2.Token, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start OAuth callback listener: %w", err)
	}
	defer listener.Close()

	stateBytes := make([]byte, 32)
	if _, err = rand.Read(stateBytes); err != nil {
		return nil, err
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)
	config.RedirectURL = "http://" + listener.Addr().String() + "/oauth2/callback"
	authURL := config.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)

	type result struct {
		token *oauth2.Token
		err   error
	}
	resultCh := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "Invalid OAuth state.", http.StatusBadRequest)
			resultCh <- result{err: errors.New("OAuth state mismatch")}
			return
		}
		if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
			http.Error(w, "Google authorization was not completed.", http.StatusBadRequest)
			resultCh <- result{err: fmt.Errorf("Google authorization: %s", oauthErr)}
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "Authorization code is missing.", http.StatusBadRequest)
			resultCh <- result{err: errors.New("OAuth authorization code missing")}
			return
		}
		token, exchangeErr := config.Exchange(r.Context(), code)
		if exchangeErr != nil {
			http.Error(w, "Token exchange failed.", http.StatusInternalServerError)
			resultCh <- result{err: fmt.Errorf("exchange OAuth code: %w", exchangeErr)}
			return
		}
		fmt.Fprintln(w, "Authorization complete. You can close this tab and return to Terminal.")
		resultCh <- result{token: token}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serveDone := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveDone <- err
	}()

	fmt.Fprintln(out, "Open this URL to authorize Google Drive access:")
	fmt.Fprintln(out, authURL)
	_ = exec.Command("open", authURL).Start()

	var authResult result
	select {
	case authResult = <-resultCh:
	case err = <-serveDone:
		if err == nil {
			err = errors.New("OAuth callback server stopped")
		}
		authResult.err = err
	case <-ctx.Done():
		authResult.err = ctx.Err()
	case <-time.After(5 * time.Minute):
		authResult.err = errors.New("OAuth authorization timed out after 5 minutes")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	return authResult.token, authResult.err
}

func listFolderTree(ctx context.Context, service *drive.Service, rootID string, out io.Writer) ([]remoteFile, error) {
	type folder struct {
		ID   string
		Path string
	}
	queue := []folder{{ID: rootID}}
	visited := map[string]bool{}
	files := make([]remoteFile, 0)
	foldersScanned := 0

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if visited[current.ID] {
			continue
		}
		visited[current.ID] = true
		foldersScanned++

		pageToken := ""
		for {
			call := service.Files.List().
				Q(fmt.Sprintf("'%s' in parents and trashed = false", current.ID)).
				Spaces("drive").
				PageSize(1000).
				Fields("nextPageToken,files(id,name,mimeType,size,md5Checksum)").
				SupportsAllDrives(true).
				IncludeItemsFromAllDrives(true).
				Context(ctx)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			result, err := call.Do()
			if err != nil {
				return nil, fmt.Errorf("list Drive folder %s: %w", current.ID, err)
			}
			for _, file := range result.Files {
				path := file.Name
				if current.Path != "" {
					path = current.Path + "/" + file.Name
				}
				if file.MimeType == "application/vnd.google-apps.folder" {
					queue = append(queue, folder{ID: file.Id, Path: path})
					continue
				}
				files = append(files, remoteFile{Path: path, Size: file.Size, MD5: strings.ToLower(file.Md5Checksum)})
			}
			pageToken = result.NextPageToken
			if pageToken == "" {
				break
			}
		}
		if foldersScanned%100 == 0 {
			fmt.Fprintf(out, "Scanned %d folders; found %d files...\n", foldersScanned, len(files))
		}
	}
	fmt.Fprintf(out, "Scanned %d folders.\n", foldersScanned)
	return files, nil
}

func printDuplicates(out io.Writer, label string, files []remoteFile) {
	groups, missingHash := findDuplicateGroups(files)
	duplicateFiles := 0
	var reclaimable uint64
	for _, group := range groups {
		duplicateFiles += len(group.Paths)
		reclaimable += uint64(group.Key.Size) * uint64(len(group.Paths)-1)
	}
	fmt.Fprintf(out, "Remote: %s\n", label)
	fmt.Fprintf(out, "Remote files: %d; usable size/MD5 records: %d; without usable MD5: %d.\n", len(files), len(files)-missingHash, missingHash)
	fmt.Fprintf(out, "Duplicate groups: %d; files in duplicate groups: %d; reclaimable if one copy per group is kept: %s.\n", len(groups), duplicateFiles, formatBytes(reclaimable))
	for i, group := range groups {
		fmt.Fprintf(out, "DUPLICATE %d: %d files; each=%s; MD5=%s\n", i+1, len(group.Paths), formatBytes(uint64(group.Key.Size)), group.Key.MD5)
		for _, path := range group.Paths {
			fmt.Fprintf(out, "  %s\n", path)
		}
	}
	if len(groups) == 0 {
		fmt.Fprintln(out, "No byte-identical remote files found.")
	}
}

func findDuplicateGroups(files []remoteFile) ([]duplicateGroup, int) {
	byContent := make(map[contentKey][]string)
	missingHash := 0
	for _, file := range files {
		if file.Size < 0 || file.MD5 == "" {
			missingHash++
			continue
		}
		key := contentKey{Size: file.Size, MD5: strings.ToLower(file.MD5)}
		byContent[key] = append(byContent[key], file.Path)
	}
	groups := make([]duplicateGroup, 0)
	for key, paths := range byContent {
		if len(paths) < 2 {
			continue
		}
		sort.Strings(paths)
		groups = append(groups, duplicateGroup{Key: key, Paths: paths})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Key.Size != groups[j].Key.Size {
			return groups[i].Key.Size > groups[j].Key.Size
		}
		return groups[i].Paths[0] < groups[j].Paths[0]
	})
	return groups, missingHash
}

func readToken(path string) (*oauth2.Token, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var token oauth2.Token
	if err = json.Unmarshal(b, &token); err != nil {
		return nil, fmt.Errorf("parse OAuth token: %w", err)
	}
	return &token, nil
}

func writeToken(path string, token *oauth2.Token) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "google-token-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		err = json.NewEncoder(tmp).Encode(token)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func resolveTokenPath(value string) (string, error) {
	if value != "" {
		return filepath.Abs(expandHome(value))
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "photos-to-drive", "google-token.json"), nil
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func formatBytes(n uint64) string {
	const unit = uint64(1024)
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		value /= float64(unit)
		if value < float64(unit) || suffix == "PiB" {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%d B", n)
}

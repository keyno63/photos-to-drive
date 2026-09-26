// drive-api-poc is a read-only proof of concept for accessing Google Drive
// directly, without rclone. Upload support is intentionally not implemented yet.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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
	"google.golang.org/api/option"
)

type options struct {
	credentials string
	token       string
	folderID    string
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

func main() {
	var o options
	flag.StringVar(&o.credentials, "credentials", "", "OAuth Desktop app client-secret JSON file")
	flag.StringVar(&o.token, "token", "", "OAuth token file (default: user config directory)")
	flag.StringVar(&o.folderID, "folder-id", "", "Google Drive folder ID to scan recursively")
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
	printDuplicates(out, "drive-folder:"+o.folderID, files)
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

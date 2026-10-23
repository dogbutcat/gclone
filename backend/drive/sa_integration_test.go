// Verify gclone's SA switching and ID paths without contacting Google Drive.
package drive

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/internal/dircache"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

func testSAFiles(t *testing.T) []string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	credentials, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "test@example.invalid",
		"private_key":  string(keyPEM),
		"token_uri":    "https://example.invalid/token",
	})
	require.NoError(t, err)
	dir := t.TempDir()
	files := []string{filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")}
	for _, name := range files {
		require.NoError(t, os.WriteFile(name, credentials, 0600))
	}
	return files
}

func testSAFs(t *testing.T, files []string) *Fs {
	t.Helper()
	f := &Fs{
		name: "test",
		opt: Options{
			ServiceAccountFile:     files[0],
			ServiceAccountFilePath: filepath.Dir(files[0]),
			V2DownloadMinSize:      -1,
		},
		ServiceAccountFiles: new(SaInfo),
		waitChangeSvc:       new(sync.Mutex),
	}
	f.ServiceAccountFiles.updateSas(files, files[0])
	return f
}

func TestSARateLimitSwitch(t *testing.T) {
	files := testSAFiles(t)
	for _, reason := range []string{"rateLimitExceeded", "userRateLimitExceeded"} {
		t.Run(reason, func(t *testing.T) {
			f := testSAFs(t, files)
			err := &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: reason}}}
			retry, gotErr := f.shouldRetry(context.Background(), err)
			assert.True(t, retry)
			assert.Same(t, err, gotErr)
			assert.Equal(t, files[1], f.opt.ServiceAccountFile)
			assert.Equal(t, 1, f.ServiceAccountFiles.activeIdx)
			assert.True(t, f.ServiceAccountFiles.sas[0].isStale)
			require.NotNil(t, f.client)
			require.NotNil(t, f.svc)
		})
	}
}

func TestSAStopOnUploadLimit(t *testing.T) {
	files := testSAFiles(t)
	f := testSAFs(t, files)
	f.opt.StopOnUploadLimit = true
	err := &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{
		Reason: "userRateLimitExceeded", Message: "User rate limit exceeded.",
	}}}
	retry, gotErr := f.shouldRetry(context.Background(), err)
	assert.False(t, retry)
	require.Error(t, gotErr)
	assert.Equal(t, files[0], f.opt.ServiceAccountFile)
	assert.Len(t, f.ServiceAccountFiles.saPool, 2)
}

func TestSARollingSwitch(t *testing.T) {
	files := testSAFiles(t)
	f := testSAFs(t, files)
	f.rollingSvc(context.Background())
	assert.Equal(t, files[1], f.opt.ServiceAccountFile)
	assert.Equal(t, 1, f.ServiceAccountFiles.activeIdx)
	assert.Len(t, f.ServiceAccountFiles.saPool, 2)
	f.rollingSvc(context.Background())
	assert.Equal(t, files[0], f.opt.ServiceAccountFile)
	assert.Equal(t, 0, f.ServiceAccountFiles.activeIdx)
}

func TestSAFailedSwitchRestoresClient(t *testing.T) {
	files := testSAFiles(t)
	f := testSAFs(t, files)
	require.NoError(t, f.changeServiceAccountFile(context.Background(), files[1]))
	oldClient, oldService, oldCredentials := f.client, f.svc, f.opt.ServiceAccountCredentials
	err := f.changeServiceAccountFile(context.Background(), filepath.Join(t.TempDir(), "missing.json"))
	require.Error(t, err)
	assert.Same(t, oldClient, f.client)
	assert.Same(t, oldService, f.svc)
	assert.Equal(t, files[1], f.opt.ServiceAccountFile)
	assert.Equal(t, oldCredentials, f.opt.ServiceAccountCredentials)
}

func TestSAIDPathAndRollingCount(t *testing.T) {
	files := testSAFiles(t)
	fileID := strings.Repeat("a", 33)
	for _, test := range []struct {
		name, path, rootID, teamID string
		maybeFile                  bool
	}{
		{"normal", "media", "", "", false},
		{"file or folder ID", "{" + fileID + "}/media", fileID, "", true},
		{"shared drive ID", "{shared-drive-id}/media", "shared-drive-id", "shared-drive-id", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, ci := fs.AddConfig(context.Background())
			f, err := newFs(ctx, "test", test.path, configmap.Simple{
				"service_account_file": files[0], "chunk_size": "256Ki",
				"rolling_sa": "true", "rolling_count": "3",
			})
			require.NoError(t, err)
			assert.Equal(t, "media", f.Root())
			assert.Equal(t, test.rootID, f.opt.RootFolderID)
			assert.Equal(t, test.teamID, f.opt.TeamDriveID)
			assert.Equal(t, test.maybeFile, f.maybeIsFile)
			assert.Equal(t, 3, ci.Transfers)
			require.NotNil(t, f.ServiceAccountFiles)
			require.NotNil(t, f.waitChangeSvc)
		})
	}
}

type saListRequest struct {
	account, resourceKeys string
	query                 url.Values
	method, path          string
	body                  []byte
}

type saFixtureResponse struct {
	body    any
	headers http.Header
}

// testSAListFixture routes OAuth and Drive requests exclusively to a local TLS server.
func testSAListFixture(t *testing.T, handle func(saListRequest) (int, any), overrides ...configmap.Simple) (context.Context, *Fs, func() []saListRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []saListRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			parts := strings.Split(r.Form.Get("assertion"), ".")
			if len(parts) != 3 {
				http.Error(w, "invalid assertion", http.StatusBadRequest)
				return
			}
			claims, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var jwt struct {
				Issuer string `json:"iss"`
			}
			if err := json.Unmarshal(claims, &jwt); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": strings.TrimSuffix(jwt.Issuer, "@example.invalid"),
				"token_type":   "Bearer", "expires_in": 3600,
			})
			return
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		request := saListRequest{
			account: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
			query:   r.URL.Query(), resourceKeys: r.Header.Get("X-Goog-Drive-Resource-Keys"),
			method: r.Method, path: r.URL.Path, body: payload,
		}
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		status, body := handle(request)
		if response, ok := body.(saFixtureResponse); ok {
			for key, values := range response.headers {
				w.Header()[key] = values
			}
			body = response.body
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "www.googleapis.com:443" {
			http.Error(w, "fixture refuses external destinations", http.StatusBadGateway)
			return
		}
		upstream, err := net.Dial("tcp", server.Listener.Addr().String())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffer.Flush()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, conn); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(conn, upstream)
		_ = conn.Close()
		<-done
	}))
	t.Cleanup(proxy.Close)
	files := testSAFiles(t)
	for i, name := range files {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		var credentials map[string]string
		require.NoError(t, json.Unmarshal(data, &credentials))
		credentials["client_email"] = fmt.Sprintf("%c@example.invalid", 'a'+i)
		credentials["token_uri"] = "https://www.googleapis.com/token"
		data, err = json.Marshal(credentials)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(name, data, 0600))
	}
	ctx, ci := fs.AddConfig(context.Background())
	ci.Checkers, ci.LowLevelRetries = 2, 2
	ci.HTTPProxy, ci.InsecureSkipVerify, ci.DisableHTTPKeepAlives = proxy.URL, true, true
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	t.Cleanup(cancel)
	m := configmap.Simple{
		"service_account_file": files[0], "service_account_file_path": filepath.Dir(files[0]),
		"scope": "drive", "chunk_size": "256Ki", "v2_download_min_size": "off",
		"pacer_min_sleep": "1ms", "pacer_burst": "100",
	}
	for _, override := range overrides {
		for key, value := range override {
			m[key] = value
		}
	}
	f, err := newFs(ctx, "fixture", "", m)
	require.NoError(t, err)
	f.rootFolderID = "root"
	f.dirCache = dircache.New("", "root", f)
	if f.ServiceAccountFiles.isPoolEmpty() {
		f.ServiceAccountFiles.updateSas(files, f.opt.ServiceAccountFile)
	}
	return ctx, f, func() []saListRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]saListRequest(nil), requests...)
	}
}

func saListQuota(reason string) *googleapi.Error {
	return &googleapi.Error{Code: 403, Message: "User rate limit exceeded.", Errors: []googleapi.ErrorItem{{
		Reason: reason, Message: "User rate limit exceeded.",
	}}}
}

func TestSAListQuotaRetry(t *testing.T) {
	for _, reason := range []string{"rateLimitExceeded", "userRateLimitExceeded"} {
		t.Run(reason, func(t *testing.T) {
			ctx, f, requests := testSAListFixture(t, func(r saListRequest) (int, any) {
				if r.account == "a" {
					return 403, map[string]any{"error": saListQuota(reason)}
				}
				return 200, &drive.FileList{Files: []*drive.File{{Id: "file", Name: "sample.txt"}}}
			})
			var names []string
			_, err := f.list(ctx, []string{"root"}, "", false, false, false, false, func(item *drive.File) bool {
				names = append(names, item.Name)
				return false
			})
			require.NoError(t, err)
			require.Equal(t, []string{"sample.txt"}, names)
			got := requests()
			require.Len(t, got, 2)
			assert.Equal(t, "a", got[0].account)
			assert.Equal(t, "b", got[1].account)
			assert.False(t, f.ServiceAccountFiles.sas[1].isStale)
		})
	}
}

func TestSAListPaginationQuota(t *testing.T) {
	for _, space := range []string{"shared drive", "appDataFolder"} {
		t.Run(space, func(t *testing.T) {
			ctx, f, requests := testSAListFixture(t, func(r saListRequest) (int, any) {
				if r.query.Get("pageToken") == "" {
					return 200, &drive.FileList{NextPageToken: "page-two", Files: []*drive.File{{Id: "one", Name: "one"}}}
				}
				if r.account == "a" {
					return 403, map[string]any{"error": saListQuota("userRateLimitExceeded")}
				}
				return 200, &drive.FileList{Files: []*drive.File{{Id: "two", Name: "two"}}}
			})
			if space == "shared drive" {
				f.isTeamDrive, f.opt.TeamDriveID = true, "shared-id"
			} else {
				f.rootFolderID = "appDataFolder"
			}
			f.opt.ListChunk = 17
			f.dirResourceKeys.Store("parent", "key")
			var names []string
			_, err := f.list(ctx, []string{"parent"}, "", false, false, false, false, func(item *drive.File) bool {
				names = append(names, item.Name)
				// A concurrent successful switch must not alter this batch's next page.
				if item.Id == "one" {
					f.waitChangeSvc.Lock()
					f.changeSvc(ctx)
					f.waitChangeSvc.Unlock()
				}
				return false
			})
			require.NoError(t, err)
			assert.Equal(t, []string{"one", "two"}, names)
			got := requests()
			require.Len(t, got, 3)
			assert.Equal(t, "a", got[0].account)
			assert.Equal(t, "a", got[1].account)
			assert.Equal(t, "b", got[2].account)
			assert.Empty(t, got[0].query.Get("pageToken"))
			assert.Equal(t, "page-two", got[1].query.Get("pageToken"))
			assert.Equal(t, got[1].query, got[2].query)
			for _, r := range got {
				assert.Equal(t, "17", r.query.Get("pageSize"))
				assert.Equal(t, "true", r.query.Get("supportsAllDrives"))
				assert.Equal(t, "true", r.query.Get("includeItemsFromAllDrives"))
				assert.Equal(t, "parent/key", r.resourceKeys)
				assert.Contains(t, r.query.Get("q"), "'parent' in parents")
				assert.Contains(t, r.query.Get("fields"), "nextPageToken")
				if space == "shared drive" {
					assert.Equal(t, "shared-id", r.query.Get("driveId"))
					assert.Equal(t, "drive", r.query.Get("corpora"))
				} else {
					assert.Equal(t, "appDataFolder", r.query.Get("spaces"))
				}
			}
			assert.Len(t, f.ServiceAccountFiles.saPool, 1)
		})
	}
}

func TestSAListDelayedQuota(t *testing.T) {
	arrived, release := make(chan struct{}, 2), make(chan struct{})
	ctx, f, requests := testSAListFixture(t, func(r saListRequest) (int, any) {
		if r.account == "a" {
			arrived <- struct{}{}
			<-release
			return 403, map[string]any{"error": saListQuota("userRateLimitExceeded")}
		}
		return 200, &drive.FileList{}
	})
	// Put the healthy account at index zero to catch absent-map-key misattribution.
	a, b := f.ServiceAccountFiles.sas[0].saPath, f.ServiceAccountFiles.sas[1].saPath
	f.ServiceAccountFiles.updateSas([]string{b, a}, a)
	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := f.list(ctx, []string{"root"}, "", false, false, false, false, func(*drive.File) bool { return false })
			done <- err
		}()
	}
	for range 2 {
		select {
		case <-arrived:
		case <-ctx.Done():
			close(release)
			t.Fatal("listing requests did not arrive", ctx.Err())
		}
	}
	f.waitChangeSvc.Lock()
	f.changeSvc(ctx)
	f.waitChangeSvc.Unlock()
	close(release)
	for range 2 {
		require.NoError(t, <-done)
	}
	got := requests()
	require.Len(t, got, 4)
	assert.Equal(t, "a", got[0].account)
	assert.Equal(t, "a", got[1].account)
	assert.Equal(t, "b", got[2].account)
	assert.Equal(t, "b", got[3].account)
	assert.Equal(t, b, f.opt.ServiceAccountFile)
	assert.Equal(t, 0, f.ServiceAccountFiles.activeIdx)
	assert.Equal(t, map[string]int{b: 0}, f.ServiceAccountFiles.saPool)
	assert.False(t, f.ServiceAccountFiles.sas[0].isStale)
}

func TestSAListRRolling(t *testing.T) {
	var mu sync.Mutex
	children := 0
	ready := make(chan struct{})
	ctx, f, requests := testSAListFixture(t, func(r saListRequest) (int, any) {
		if strings.Contains(r.query.Get("q"), "'root' in parents") {
			return 200, &drive.FileList{Files: []*drive.File{
				{Id: "left", Name: "left", MimeType: driveFolderType, Parents: []string{"root"}},
				{Id: "right", Name: "right", MimeType: driveFolderType, Parents: []string{"root"}},
			}}
		}
		mu.Lock()
		children++
		if children == 2 {
			close(ready)
		}
		mu.Unlock()
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			return 500, map[string]string{"error": "recursive workers did not overlap"}
		}
		parent := "left"
		if strings.Contains(r.query.Get("q"), "'right' in parents") {
			parent = "right"
		}
		return 200, &drive.FileList{Files: []*drive.File{{Id: parent + "-file", Name: "sample.txt",
			MimeType: "application/octet-stream", Md5Checksum: "9dd4e461268c8034f5c8564e155c67a6",
			Size: 1, Parents: []string{parent}, ModifiedTime: "2026-10-02T00:00:00Z"}}}
	})
	f.opt.RollingSA, f.grouping = true, 1
	var names []string
	err := f.ListR(ctx, "", func(entries fs.DirEntries) error {
		for _, entry := range entries {
			names = append(names, entry.Remote())
		}
		return nil
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"left", "right", "left/sample.txt", "right/sample.txt"}, names)
	got := requests()
	require.Len(t, got, 3)
	assert.Equal(t, "b", got[0].account)
	assert.ElementsMatch(t, []string{"a", "b"}, []string{got[1].account, got[2].account})
	assert.Len(t, f.ServiceAccountFiles.saPool, 2)
}

func TestSAListPPageRolling(t *testing.T) {
	ctx, f, requests := testSAListFixture(t, func(r saListRequest) (int, any) {
		if r.query.Get("pageToken") == "" {
			return 200, &drive.FileList{NextPageToken: "next"}
		}
		return 200, &drive.FileList{}
	})
	f.opt.RollingSA = true
	require.NoError(t, f.ListP(ctx, "", func(fs.DirEntries) error { return nil }))
	got := requests()
	require.Len(t, got, 2)
	assert.Equal(t, "b", got[0].account)
	assert.Equal(t, "b", got[1].account)
	assert.Equal(t, "next", got[1].query.Get("pageToken"))
}

func TestSAListStopOnUploadLimit(t *testing.T) {
	ctx, f, requests := testSAListFixture(t, func(saListRequest) (int, any) {
		return 403, map[string]any{"error": saListQuota("userRateLimitExceeded")}
	})
	f.opt.StopOnUploadLimit = true
	_, err := f.list(ctx, []string{"root"}, "", false, false, false, false, func(*drive.File) bool { return false })
	require.Error(t, err)
	assert.Len(t, requests(), 1)
	assert.Len(t, f.ServiceAccountFiles.saPool, 2)
	assert.False(t, f.ServiceAccountFiles.sas[0].isStale)
}

func TestSAListNonQuotaRetryPinsAccount(t *testing.T) {
	var f *Fs
	attempt := 0
	ctx, fixture, requests := testSAListFixture(t, func(saListRequest) (int, any) {
		attempt++
		if attempt == 1 {
			f.waitChangeSvc.Lock()
			f.changeSvc(context.Background())
			f.waitChangeSvc.Unlock()
			return 503, map[string]any{"error": &googleapi.Error{Code: 503, Message: "Unavailable"}}
		}
		return 200, &drive.FileList{}
	})
	f = fixture
	_, err := f.list(ctx, []string{"root"}, "", false, false, false, false, func(*drive.File) bool { return false })
	require.NoError(t, err)
	got := requests()
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].account)
	assert.Equal(t, "a", got[1].account)
	assert.Equal(t, f.ServiceAccountFiles.sas[1].saPath, f.opt.ServiceAccountFile)
}

func TestSAQuotaFailedSwitchRestoresPool(t *testing.T) {
	files := testSAFiles(t)
	f := testSAFs(t, files)
	f.opt.ServiceAccountFile = ""
	require.NoError(t, f.changeServiceAccountFile(context.Background(), files[0]))
	client, service, credentials := f.client, f.svc, f.opt.ServiceAccountCredentials
	require.NoError(t, os.Remove(files[1]))
	f.changeSvc(context.Background())
	assert.Same(t, client, f.client)
	assert.Same(t, service, f.svc)
	assert.Equal(t, credentials, f.opt.ServiceAccountCredentials)
	assert.Equal(t, files[0], f.opt.ServiceAccountFile)
	assert.Equal(t, 0, f.ServiceAccountFiles.activeIdx)
	assert.False(t, f.ServiceAccountFiles.sas[0].isStale)
	// The committed client is retained, but the missing candidate is isolated.
	assert.Equal(t, map[string]int{files[0]: 0}, f.ServiceAccountFiles.saPool)
	assert.True(t, f.ServiceAccountFiles.invalid[files[1]])
}

func TestSARollingSingleAccount(t *testing.T) {
	files := testSAFiles(t)
	f := testSAFs(t, files[:1])
	f.opt.ServiceAccountFile = ""
	require.NoError(t, f.changeServiceAccountFile(context.Background(), files[0]))
	client, service := f.client, f.svc
	f.rollingSvc(context.Background())
	assert.Same(t, client, f.client)
	assert.Same(t, service, f.svc)
	assert.Equal(t, files[0], f.opt.ServiceAccountFile)
	assert.Equal(t, 0, f.ServiceAccountFiles.activeIdx)
}

func TestSARandomStartActiveAccount(t *testing.T) {
	files := testSAFiles(t)
	for range 16 {
		f, err := newFs(context.Background(), "test", "", configmap.Simple{
			"service_account_file": files[0], "service_account_file_path": filepath.Dir(files[0]),
			"chunk_size": "256Ki", "random_pick_sa": "true",
		})
		require.NoError(t, err)
		assert.Equal(t, f.opt.ServiceAccountFile, f.ServiceAccountFiles.sas[f.ServiceAccountFiles.activeIdx].saPath)
	}
}

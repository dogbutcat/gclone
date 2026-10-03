// Verify directory-only SA startup and concurrent request ownership with local fixtures.
package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/fs/operations"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/drive/v3"
)

func TestSARandomDirectoryStartup(t *testing.T) {
	files := testSAFiles(t)
	pool := t.TempDir()
	selected := filepath.Join(pool, "selected.json")
	credentials, err := os.ReadFile(files[0])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(selected, credentials, 0600))
	require.NoError(t, os.Mkdir(filepath.Join(pool, "not-an-account.json"), 0700))
	for _, test := range []struct{ name, initial string }{
		{"omitted", ""}, {"missing", filepath.Join(pool, "missing.json")},
		{"external", files[1]}, {"in pool", selected},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, err := newFs(context.Background(), "test", "", configmap.Simple{
				"service_account_file": test.initial, "service_account_file_path": pool,
				"chunk_size": "256Ki", "random_pick_sa": "true",
			})
			require.NoError(t, err)
			assert.Equal(t, selected, f.opt.ServiceAccountFile)
			assert.Equal(t, string(credentials), f.opt.ServiceAccountCredentials)
			assert.Equal(t, map[string]int{selected: 0}, f.ServiceAccountFiles.saPool)
			assert.Equal(t, 0, f.ServiceAccountFiles.activeIdx)
		})
	}
	t.Setenv("GCLONE_TEST_SA_DIRECTORY", pool)
	f, err := newFs(context.Background(), "test", "", configmap.Simple{
		"service_account_file_path": "$GCLONE_TEST_SA_DIRECTORY", "random_pick_sa": "true", "chunk_size": "256Ki",
	})
	require.NoError(t, err)
	assert.Equal(t, selected, f.opt.ServiceAccountFile)
}

func TestSARandomDirectoryErrors(t *testing.T) {
	files := testSAFiles(t)
	empty, directoryOnly, malformed := t.TempDir(), t.TempDir(), t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(directoryOnly, "account.json"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(malformed, "account.json"), []byte("invalid-json"), 0600))
	for _, test := range []struct{ name, directory, message string }{
		{"unspecified", "", "requires service_account_file_path"},
		{"missing", filepath.Join(empty, "missing"), "read service account directory"},
		{"file", files[0], "read service account directory"},
		{"empty", empty, "contains no JSON files"},
		{"directory suffix", directoryOnly, "contains no JSON files"},
		{"malformed", malformed, "credentials"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := newFs(context.Background(), "test", "", configmap.Simple{
				"service_account_file": files[0], "service_account_file_path": test.directory,
				"random_pick_sa": "true", "chunk_size": "256Ki",
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.message)
		})
	}
}

func TestSARandomUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read permission-restricted directories")
	}
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0000))
	t.Cleanup(func() { _ = os.Chmod(directory, 0700) })
	_, err := newFs(context.Background(), "test", "", configmap.Simple{
		"service_account_file_path": directory, "random_pick_sa": "true", "chunk_size": "256Ki",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read service account directory")
}

func TestSARandomInlineIdentity(t *testing.T) {
	files := testSAFiles(t)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	var inline map[string]string
	require.NoError(t, json.Unmarshal(data, &inline))
	inline["client_email"], inline["token_uri"] = "inline@example.invalid", "https://www.googleapis.com/token"
	data, err = json.Marshal(inline)
	require.NoError(t, err)
	for _, random := range []bool{false, true} {
		t.Run(fmt.Sprint(random), func(t *testing.T) {
			ctx, f, requests := testSAListFixture(t, func(saListRequest) (int, any) { return 200, &drive.FileList{} }, configmap.Simple{
				"random_pick_sa": fmt.Sprint(random), "service_account_credentials": string(data),
			})
			_, err := f.list(ctx, []string{"root"}, "", false, false, false, false, func(*drive.File) bool { return false })
			require.NoError(t, err)
			got := requests()
			require.Len(t, got, 1)
			if random {
				assert.Equal(t, strings.TrimSuffix(filepath.Base(f.opt.ServiceAccountFile), ".json"), got[0].account)
			} else {
				assert.Equal(t, "inline", got[0].account)
			}
		})
	}
}

func saRuntimeFile(id, name string) *drive.File {
	return &drive.File{Id: id, Name: name, MimeType: "application/octet-stream", Size: 1,
		Md5Checksum: "9dd4e461268c8034f5c8564e155c67a6", Parents: []string{"root"}, ModifiedTime: "2026-10-03T00:00:00Z"}
}

func waitSAWorkers(t *testing.T, ctx context.Context, arrived <-chan struct{}, release chan struct{}) {
	t.Helper()
	for range 4 {
		select {
		case <-arrived:
		case <-ctx.Done():
			close(release)
			t.Fatal("four request workers did not overlap", ctx.Err())
		}
	}
}

func switchSAWhileRequestsWait(t *testing.T, ctx context.Context, f *Fs, initial string) string {
	t.Helper()
	next := ""
	for _, entry := range f.ServiceAccountFiles.sas {
		if entry.saPath != initial {
			next = entry.saPath
		}
	}
	require.NotEmpty(t, next)
	require.NoError(t, f.changeServiceAccountFile(ctx, next))
	f.waitChangeSvc.Lock()
	f.ServiceAccountFiles.activeSa(next)
	f.waitChangeSvc.Unlock()
	return next
}

func TestSAFourCopyWorkers(t *testing.T) {
	arrived, release := make(chan struct{}, 4), make(chan struct{})
	initialAccount := ""
	ctx, f, requests := testSAListFixture(t, func(r saListRequest) (int, any) {
		if r.method != "POST" {
			return 200, &drive.FileList{}
		}
		if r.account == initialAccount {
			arrived <- struct{}{}
			<-release
			return 403, map[string]any{"error": saListQuota("userRateLimitExceeded")}
		}
		var info drive.File
		if err := json.Unmarshal(r.body, &info); err != nil {
			return 400, map[string]string{"error": err.Error()}
		}
		return 200, saRuntimeFile("copied-"+info.Name, info.Name)
	}, configmap.Simple{"random_pick_sa": "true", "rolling_sa": "true", "rolling_count": "4"})
	initial := f.opt.ServiceAccountFile
	initialAccount = strings.TrimSuffix(filepath.Base(initial), ".json")
	require.Equal(t, 4, f.ci.Transfers)
	done := make(chan error, 4)
	for i := range 4 {
		src, err := f.newObjectWithInfo(ctx, fmt.Sprintf("source-%d", i), saRuntimeFile(fmt.Sprintf("source-%d", i), fmt.Sprintf("source-%d", i)))
		require.NoError(t, err)
		go func(i int, src fs.Object) { _, err := f.Copy(ctx, src, fmt.Sprintf("copy-%d", i)); done <- err }(i, src)
	}
	waitSAWorkers(t, ctx, arrived, release)
	next := switchSAWhileRequestsWait(t, ctx, f, initial)
	close(release)
	for range 4 {
		require.NoError(t, <-done)
	}
	var posts []saListRequest
	for _, r := range requests() {
		if r.method == "POST" {
			posts = append(posts, r)
		}
	}
	require.Len(t, posts, 8)
	for _, r := range posts[:4] {
		assert.Equal(t, initialAccount, r.account)
	}
	for _, r := range posts[4:] {
		assert.Equal(t, strings.TrimSuffix(filepath.Base(next), ".json"), r.account)
	}
	assert.Equal(t, next, f.opt.ServiceAccountFile)
	assert.Len(t, f.ServiceAccountFiles.saPool, 1)
	assert.Contains(t, f.ServiceAccountFiles.saPool, next)
}

func TestSAFourUploadWorkers(t *testing.T) {
	arrived, release := make(chan struct{}, 4), make(chan struct{})
	initialAccount := ""
	ctx, f, requests := testSAListFixture(t, func(r saListRequest) (int, any) {
		if r.path == "/upload/drive/v3/files" {
			if r.account == initialAccount {
				arrived <- struct{}{}
				<-release
				return 403, map[string]any{"error": saListQuota("userRateLimitExceeded")}
			}
			var info drive.File
			if err := json.Unmarshal(r.body, &info); err != nil {
				return 400, map[string]string{"error": err.Error()}
			}
			return 200, saFixtureResponse{body: map[string]string{}, headers: http.Header{"Location": {"https://www.googleapis.com/upload/session/" + info.Name}}}
		}
		return 200, saRuntimeFile("uploaded", filepath.Base(r.path))
	}, configmap.Simple{"random_pick_sa": "true", "rolling_sa": "true", "rolling_count": "4"})
	initial := f.opt.ServiceAccountFile
	initialAccount = strings.TrimSuffix(filepath.Base(initial), ".json")
	require.Equal(t, 4, f.ci.Transfers)
	done := make(chan error, 4)
	for i := range 4 {
		go func(i int) {
			name := fmt.Sprintf("upload-%d", i)
			_, err := f.Upload(ctx, strings.NewReader("x"), 1, "text/plain", "", name, &drive.File{Name: name})
			done <- err
		}(i)
	}
	waitSAWorkers(t, ctx, arrived, release)
	next := switchSAWhileRequestsWait(t, ctx, f, initial)
	close(release)
	for range 4 {
		require.NoError(t, <-done)
	}
	var starts, chunks []saListRequest
	for _, r := range requests() {
		if r.path == "/upload/drive/v3/files" {
			starts = append(starts, r)
		} else {
			chunks = append(chunks, r)
		}
	}
	require.Len(t, starts, 8)
	require.Len(t, chunks, 4)
	for _, r := range starts[:4] {
		assert.Equal(t, initialAccount, r.account)
	}
	for _, r := range append(starts[4:], chunks...) {
		assert.Equal(t, strings.TrimSuffix(filepath.Base(next), ".json"), r.account)
	}
	assert.Len(t, f.ServiceAccountFiles.saPool, 1)
	assert.Contains(t, f.ServiceAccountFiles.saPool, next)
}

func TestSAUploadSessionPinsAccount(t *testing.T) {
	var f *Fs
	chunks := 0
	ctx, fixture, requests := testSAListFixture(t, func(r saListRequest) (int, any) {
		if r.path == "/upload/drive/v3/files" {
			next := f.ServiceAccountFiles.sas[1].saPath
			if err := f.changeServiceAccountFile(context.Background(), next); err != nil {
				return 500, map[string]string{"error": err.Error()}
			}
			return 200, saFixtureResponse{body: map[string]string{}, headers: http.Header{"Location": {"https://www.googleapis.com/upload/session/pinned"}}}
		}
		chunks++
		if chunks == 1 {
			return 503, map[string]any{"error": map[string]any{"code": 503, "message": "Unavailable"}}
		}
		if chunks == 2 {
			return 308, map[string]string{}
		}
		return 200, saRuntimeFile("uploaded", "pinned")
	})
	f = fixture
	data := bytes.Repeat([]byte("x"), int(f.opt.ChunkSize)+1)
	_, err := f.Upload(ctx, bytes.NewReader(data), int64(len(data)), "text/plain", "", "pinned", &drive.File{Name: "pinned"})
	require.NoError(t, err)
	got := requests()
	require.Len(t, got, 4)
	for _, r := range got {
		assert.Equal(t, "a", r.account)
	}
	assert.Equal(t, got[1].body, got[2].body)
	assert.Equal(t, data[:len(data)-1], got[1].body)
	assert.Equal(t, data[len(data)-1:], got[3].body)
	assert.Equal(t, f.ServiceAccountFiles.sas[1].saPath, f.opt.ServiceAccountFile)
}

func TestSAUploadChunkQuotaRestartsTransfer(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			ctx, f, requests := testSAListFixture(t, func(r saListRequest) (int, any) {
				if r.path == "/upload/drive/v3/files" {
					return 200, saFixtureResponse{body: map[string]string{}, headers: http.Header{"Location": {"https://www.googleapis.com/upload/session/quota"}}}
				}
				return 403, map[string]any{"error": saListQuota("userRateLimitExceeded")}
			})
			f.opt.StopOnUploadLimit = stop
			_, err := f.Upload(ctx, strings.NewReader("x"), 1, "text/plain", "", "quota", &drive.File{Name: "quota"})
			require.Error(t, err)
			got := requests()
			require.Len(t, got, 2)
			assert.Equal(t, "a", got[1].account)
			if stop {
				assert.True(t, fserrors.IsFatalError(err))
				assert.Len(t, f.ServiceAccountFiles.saPool, 2)
			} else {
				assert.True(t, fserrors.IsRetryError(err))
				assert.Equal(t, f.ServiceAccountFiles.sas[1].saPath, f.opt.ServiceAccountFile)
			}
		})
	}
}

func TestSAPutRandomRollingSession(t *testing.T) {
	ctx, f, requests := testSAListFixture(t, func(r saListRequest) (int, any) {
		if r.method == "GET" {
			return 200, &drive.FileList{}
		}
		if r.path == "/upload/drive/v3/files" {
			return 200, saFixtureResponse{body: map[string]string{}, headers: http.Header{"Location": {"https://www.googleapis.com/upload/session/put"}}}
		}
		return 200, saRuntimeFile("put", "put.txt")
	}, configmap.Simple{"random_pick_sa": "true", "rolling_sa": "true", "rolling_count": "4", "upload_cutoff": "0"})
	initial := f.opt.ServiceAccountFile
	expected := ""
	for _, sa := range f.ServiceAccountFiles.sas {
		if sa.saPath != initial {
			expected = strings.TrimSuffix(filepath.Base(sa.saPath), ".json")
		}
	}
	src := object.NewStaticObjectInfo("put.txt", time.Now(), 1, true, nil, f)
	_, err := f.Put(ctx, strings.NewReader("x"), src)
	require.NoError(t, err)
	got := requests()
	require.NotEmpty(t, got)
	for _, r := range got {
		assert.Equal(t, expected, r.account)
	}
	assert.Equal(t, 4, f.ci.Transfers)
}

func TestSAUploadQuotaCopyReopensSession(t *testing.T) {
	sessions := 0
	ctx, f, requests := testSAListFixture(t, func(r saListRequest) (int, any) {
		if r.method == "GET" {
			return 200, &drive.FileList{}
		}
		if r.path == "/upload/drive/v3/files" {
			sessions++
			return 200, saFixtureResponse{body: map[string]string{}, headers: http.Header{"Location": {fmt.Sprintf("https://www.googleapis.com/upload/session/%d", sessions)}}}
		}
		if r.path == "/upload/session/1" {
			return 403, map[string]any{"error": saListQuota("userRateLimitExceeded")}
		}
		return 200, saRuntimeFile("copied", "upload.txt")
	}, configmap.Simple{"upload_cutoff": "0"})
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "upload.txt"), []byte("x"), 0600))
	local, err := fs.NewFs(ctx, source)
	require.NoError(t, err)
	obj, err := local.NewObject(ctx, "upload.txt")
	require.NoError(t, err)
	_, err = operations.Copy(ctx, f, nil, "upload.txt", obj)
	require.NoError(t, err)
	assert.Equal(t, 2, sessions)
	var chunks []saListRequest
	for _, r := range requests() {
		if strings.HasPrefix(r.path, "/upload/session/") {
			chunks = append(chunks, r)
		}
	}
	require.Len(t, chunks, 2)
	assert.Equal(t, "/upload/session/1", chunks[0].path)
	assert.Equal(t, "a", chunks[0].account)
	assert.Equal(t, "/upload/session/2", chunks[1].path)
	assert.Equal(t, "b", chunks[1].account)
	for _, r := range chunks {
		assert.Equal(t, []byte("x"), r.body)
	}
}

func TestSAExplicitSwitchRestoresV2(t *testing.T) {
	files := testSAFiles(t)
	f := testSAFs(t, files)
	f.opt.V2DownloadMinSize = 0
	require.NoError(t, f.changeServiceAccountFile(context.Background(), files[1]))
	client, service, v2, credentials := f.client, f.svc, f.v2Svc, f.opt.ServiceAccountCredentials
	require.NotNil(t, v2)
	require.Error(t, f.changeServiceAccountFile(context.Background(), filepath.Join(t.TempDir(), "missing.json")))
	assert.Same(t, client, f.client)
	assert.Same(t, service, f.svc)
	assert.Same(t, v2, f.v2Svc)
	assert.Equal(t, credentials, f.opt.ServiceAccountCredentials)
	assert.Equal(t, files[1], f.opt.ServiceAccountFile)
}

type saCountingReader struct {
	io.Reader
	bytes int
}

func (r *saCountingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes += n
	return n, err
}

func TestSAMultipartDoesNotReplayInput(t *testing.T) {
	ctx, f, requests := testSAListFixture(t, func(r saListRequest) (int, any) {
		if r.method == "GET" {
			return 200, &drive.FileList{}
		}
		return 403, map[string]any{"error": saListQuota("userRateLimitExceeded")}
	}, configmap.Simple{"upload_cutoff": "8Mi"})
	reader := &saCountingReader{Reader: strings.NewReader("contents")}
	src := object.NewStaticObjectInfo("small.txt", time.Now(), 8, true, nil, f)
	_, err := f.Put(ctx, reader, src)
	require.Error(t, err)
	assert.Equal(t, 8, reader.bytes)
	posts := 0
	for _, r := range requests() {
		if r.method == "POST" {
			posts++
		}
	}
	assert.Equal(t, 1, posts)
}

func TestSAMetadataAndQueryQuota(t *testing.T) {
	for _, labels := range []bool{false, true} {
		t.Run(fmt.Sprint(labels), func(t *testing.T) {
			ctx, f, requests := testSAListFixture(t, func(r saListRequest) (int, any) {
				if r.account == "a" {
					return 403, map[string]any{"error": saListQuota("userRateLimitExceeded")}
				}
				if labels {
					return 200, &drive.LabelList{Labels: []*drive.Label{{Id: "label"}}}
				}
				return 200, &drive.FileList{Files: []*drive.File{saRuntimeFile("file", "file")}}
			})
			if labels {
				got, err := f.getLabels(ctx, "file")
				require.NoError(t, err)
				require.Len(t, got, 1)
			} else {
				items := 0
				err := f.queryFn(ctx, "trashed=false", func(*drive.File) { items++ })
				require.NoError(t, err)
				assert.Equal(t, 1, items)
			}
			got := requests()
			require.Len(t, got, 2)
			assert.Equal(t, "a", got[0].account)
			assert.Equal(t, "b", got[1].account)
		})
	}
}

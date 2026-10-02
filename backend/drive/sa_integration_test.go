// Verify gclone's SA switching and ID paths without contacting Google Drive.
package drive

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

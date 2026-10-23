// Exercise strategy wiring through real OAuth/Drive loopback requests and failures.
package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dogbutcat/gclone/internal/dircache"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/drive/v3"
)

func crossFolderFixture(t *testing.T, handle func(saListRequest) (int, any), extra configmap.Simple) (context.Context, *Fs, []string, func() []saListRequest) {
	t.Helper()
	ctx, original, requests := testSAListFixture(t, handle)
	fs.GetConfig(ctx).LowLevelRetries = 4
	data, err := os.ReadFile(original.currentService().file)
	require.NoError(t, err)
	var credentials map[string]string
	require.NoError(t, json.Unmarshal(data, &credentials))
	root := t.TempDir()
	paths := []string{}
	for i, name := range []string{"p1/a.json", "p1/b.json", "p2/a.json", "p2/nested/b.json"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
		credentials["client_email"] = fmt.Sprintf("%c@example.invalid", 'a'+i)
		raw, err := json.Marshal(credentials)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, raw, 0600))
		paths = append(paths, path)
	}
	m := configmap.Simple{"service_account_file": paths[0], "service_account_file_path": root,
		"service_account_file_strategy": "cross-folder", "scope": "drive", "rolling_sa": "true",
		"rolling_count": "4", "chunk_size": "256Ki", "v2_download_min_size": "off", "pacer_min_sleep": "1ms", "pacer_burst": "100"}
	for key, value := range extra {
		m[key] = value
	}
	f, err := newFs(ctx, "cross-folder-fixture", "", m)
	require.NoError(t, err)
	f.rootFolderID = "root"
	f.dirCache = dircache.New("", "root", f)
	require.NoError(t, f.ServiceAccountFiles.loadInfoFromDir(root, f.currentService().file))
	return ctx, f, paths, requests
}

func strategyList(t *testing.T, ctx context.Context, f *Fs, rolling bool) {
	t.Helper()
	_, err := f.listWithSA(ctx, []string{"root"}, "", false, false, false, false, rolling, func(*drive.File) bool { return false })
	require.NoError(t, err)
}

func TestSACrossFolderRequests(t *testing.T) {
	ctx, f, paths, requests := crossFolderFixture(t, func(saListRequest) (int, any) { return 200, &drive.FileList{} }, nil)
	for _, index := range []int{2, 1, 3, 0, 2} {
		strategyList(t, ctx, f, true)
		require.Equal(t, paths[index], f.currentService().file)
	}
	got := []string{}
	for _, request := range requests() {
		got = append(got, request.account)
	}
	require.Equal(t, []string{"c", "b", "d", "a", "c"}, got)
	require.Equal(t, 4, f.ci.Transfers)
}

func TestSACrossFolderQuotaRequests(t *testing.T) {
	for _, reason := range []string{"rateLimitExceeded", "userRateLimitExceeded"} {
		t.Run(reason, func(t *testing.T) {
			ctx, f, paths, requests := crossFolderFixture(t, func(r saListRequest) (int, any) {
				if r.account == "a" || r.account == "c" {
					return 403, map[string]any{"error": saListQuota(reason)}
				}
				return 200, &drive.FileList{}
			}, nil)
			strategyList(t, ctx, f, false)
			got := []string{}
			for _, request := range requests() {
				got = append(got, request.account)
			}
			require.Equal(t, []string{"a", "c", "b"}, got)
			require.Equal(t, paths[1], f.currentService().file)
			require.NotContains(t, f.ServiceAccountFiles.saPool, paths[0])
			require.NotContains(t, f.ServiceAccountFiles.saPool, paths[2])
			require.Contains(t, f.ServiceAccountFiles.saPool, paths[3], "quota does not quarantine a whole group")
			f.waitChangeSvc.Lock()
			f.changeSvcForSA(ctx, paths[0])
			f.waitChangeSvc.Unlock()
			require.Equal(t, paths[1], f.currentService().file, "late old-account failure preserves current client")
		})
	}
}

func TestSACrossFolderBadCredentialFallback(t *testing.T) {
	for _, quota := range []bool{false, true} {
		for _, allOtherBad := range []bool{false, true} {
			t.Run(fmt.Sprintf("quota=%v/allOtherBad=%v", quota, allOtherBad), func(t *testing.T) {
				ctx, f, paths, _ := crossFolderFixture(t, func(r saListRequest) (int, any) {
					if quota && r.account == "a" {
						return 403, map[string]any{"error": saListQuota("userRateLimitExceeded")}
					}
					return 200, &drive.FileList{}
				}, nil)
				require.NoError(t, os.WriteFile(paths[2], []byte("broken"), 0600))
				want := paths[3]
				if allOtherBad {
					require.NoError(t, os.Remove(paths[3]))
					want = paths[1]
				}
				strategyList(t, ctx, f, !quota)
				require.Equal(t, want, f.currentService().file)
				require.True(t, f.ServiceAccountFiles.invalid[paths[2]])
				f.ServiceAccountFiles.revertStaleSa(paths[2])
				require.NotContains(t, f.ServiceAccountFiles.saPool, paths[2])
			})
		}
	}
}

func TestSACrossFolderCancellationAndFailedSwitch(t *testing.T) {
	ctx, f, paths, _ := crossFolderFixture(t, func(saListRequest) (int, any) { return 200, &drive.FileList{} }, nil)
	before := f.currentService()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	f.rollingSvc(cancelled)
	require.Equal(t, before, f.currentService())
	require.Equal(t, paths[2], f.ServiceAccountFiles.rollup())
	for _, path := range paths[1:] {
		require.NoError(t, os.Remove(path))
	}
	f.waitChangeSvc.Lock()
	f.changeSvcForSA(ctx, paths[0])
	f.waitChangeSvc.Unlock()
	require.Equal(t, before, f.currentService())
	require.Equal(t, paths[0], f.ServiceAccountFiles.sas[f.ServiceAccountFiles.activeIdx].saPath)
	require.Contains(t, f.ServiceAccountFiles.saPool, paths[0])
}

func TestSACrossFolderConfiguration(t *testing.T) {
	for _, config := range []configmap.Simple{
		{"service_account_file_strategy": "typo"},
		{"service_account_file_strategy": "cross-folder"},
	} {
		_, err := newFs(context.Background(), "invalid-strategy", "", config)
		require.ErrorContains(t, err, "drive:")
		require.Contains(t, err.Error(), "strategy")
	}
	ctx, f, _, _ := crossFolderFixture(t, func(saListRequest) (int, any) { return 200, &drive.FileList{} }, configmap.Simple{"random_pick_sa": "true", "service_account_file": ""})
	before, err := filepath.Rel(f.opt.ServiceAccountFilePath, f.currentService().file)
	require.NoError(t, err)
	strategyList(t, ctx, f, true)
	after, err := filepath.Rel(f.opt.ServiceAccountFilePath, f.currentService().file)
	require.NoError(t, err)
	require.NotEqual(t, strings.Split(before, string(filepath.Separator))[0], strings.Split(after, string(filepath.Separator))[0])
}

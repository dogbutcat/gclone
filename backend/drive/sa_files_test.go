// Regress recursive SA discovery, absolute identity and local credential isolation.
package drive

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dogbutcat/gclone/internal/dircache"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/drive/v3"
)

func TestSARecursiveSameBasename(t *testing.T) {
	files := testSAFiles(t)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	root := t.TempDir()
	want := []string{}
	for _, dir := range []string{"project1", "project2/nested"} {
		name := filepath.Join(root, dir, "file.json")
		require.NoError(t, os.MkdirAll(filepath.Dir(name), 0700))
		require.NoError(t, os.WriteFile(name, data, 0600))
		want = append(want, name)
	}
	var pool SaInfo
	require.NoError(t, pool.loadInfoFromDir(root, want[1]))
	require.Len(t, pool.sas, 2)
	require.Equal(t, want[0], pool.sas[0].saPath)
	require.Equal(t, want[1], pool.sas[1].saPath)
	require.Equal(t, 1, pool.activeIdx)
	require.Equal(t, want[0], pool.rollup())
	pool.markStaleSa(want[0])
	require.Contains(t, pool.saPool, want[1])
}

func TestSARecursiveRequestQuotaAndRolling(t *testing.T) {
	ctx, original, requests := testSAListFixture(t, func(r saListRequest) (int, any) {
		if r.account == "b" {
			return 403, map[string]any{"error": saListQuota("userRateLimitExceeded")}
		}
		return 200, &drive.FileList{}
	})
	poolRoot := t.TempDir()
	paths := []string{}
	for i := 0; i < 2; i++ {
		old := original.ServiceAccountFiles.sas[i].saPath
		data, err := os.ReadFile(old)
		require.NoError(t, err)
		name := filepath.Join(poolRoot, []string{"project1", "project2"}[i], "file.json")
		require.NoError(t, os.MkdirAll(filepath.Dir(name), 0700))
		require.NoError(t, os.WriteFile(name, data, 0600))
		paths = append(paths, name)
	}
	f, err := newFs(ctx, "nested-fixture", "", configmap.Simple{"service_account_file": paths[0], "service_account_file_path": poolRoot, "rolling_sa": "true", "rolling_count": "4", "scope": "drive", "chunk_size": "256Ki", "v2_download_min_size": "off", "pacer_min_sleep": "1ms", "pacer_burst": "100"})
	require.NoError(t, err)
	f.rootFolderID = "root"
	f.dirCache = dircache.New("", "root", f)
	_, err = f.listWithSA(ctx, []string{"root"}, "", false, false, false, false, true, func(*drive.File) bool { return false })
	require.NoError(t, err)
	got := requests()
	require.Len(t, got, 2)
	require.Equal(t, "b", got[0].account)
	require.Equal(t, "a", got[1].account)
	require.Equal(t, paths[0], f.currentService().file)
	require.Equal(t, map[string]int{paths[0]: 0}, f.ServiceAccountFiles.saPool)
	require.Equal(t, 4, f.ci.Transfers)
}

func TestSARecursiveUnreadableSubdirectoryPreservesPool(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("requires Unix directory permission enforcement")
	}
	files := testSAFiles(t)
	var pool SaInfo
	root := filepath.Dir(files[0])
	require.NoError(t, pool.loadInfoFromDir(root, files[0]))
	before := pool.saPool
	denied := filepath.Join(root, "denied")
	require.NoError(t, os.Mkdir(denied, 0700))
	require.NoError(t, os.Chmod(denied, 0))
	t.Cleanup(func() { _ = os.Chmod(denied, 0700) })
	require.ErrorContains(t, pool.loadInfoFromDir(root, files[0]), "read service account directory")
	require.Equal(t, before, pool.saPool)
}

func TestSACredentialValidationAndRedaction(t *testing.T) {
	files := testSAFiles(t)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	var valid map[string]string
	require.NoError(t, json.Unmarshal(data, &valid))
	block, _ := pem.Decode([]byte(valid["private_key"]))
	require.NotNil(t, block)
	rsaKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	require.NoError(t, err)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	require.NoError(t, err)
	valid["private_key"] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
	pkcs8JSON, err := json.Marshal(valid)
	require.NoError(t, err)
	require.NoError(t, validateServiceAccountJSON(pkcs8JSON))
	for _, field := range []string{"type", "client_email", "private_key"} {
		t.Run(field, func(t *testing.T) {
			changed := map[string]string{}
			for k, v := range valid {
				changed[k] = v
			}
			changed[field] = "SECRET_INVALID_VALUE"
			if field == "client_email" {
				changed[field] = ""
			}
			raw, err := json.Marshal(changed)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(files[1], raw, 0600))
			_, err = readServiceAccountFile(files[1])
			require.Error(t, err)
			require.NotContains(t, err.Error(), "SECRET_INVALID_VALUE")
			require.NotContains(t, err.Error(), valid["private_key"])
		})
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	valid["private_key"] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	raw, err := json.Marshal(valid)
	require.NoError(t, err)
	require.ErrorContains(t, validateServiceAccountJSON(raw), "RSA")
	require.NoError(t, validateServiceAccountJSON(data))
	var fields map[string]any
	require.NoError(t, json.Unmarshal(data, &fields))
	for _, field := range []string{"private_key_id", "token_uri", "project_id"} {
		original := fields[field]
		fields[field] = []string{"SECRET_INVALID_FIELD"}
		raw, err := json.Marshal(fields)
		require.NoError(t, err)
		err = validateServiceAccountJSON(raw)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "SECRET_INVALID_FIELD")
		if original == nil {
			delete(fields, field)
		} else {
			fields[field] = original
		}
	}
	fields["token_uri"] = "SECRET_INVALID_URL"
	raw, err = json.Marshal(fields)
	require.NoError(t, err)
	require.ErrorContains(t, validateServiceAccountJSON(raw), "invalid token_uri")
}

func TestSARecursiveFailureKeepsPool(t *testing.T) {
	files := testSAFiles(t)
	var pool SaInfo
	require.NoError(t, pool.loadInfoFromDir(filepath.Dir(files[0]), files[0]))
	pool.markStaleSa(files[1])
	original := pool.sas
	originalPool := pool.saPool
	active := pool.activeIdx
	bad := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bad, "bad.json"), []byte("not-json"), 0600))
	require.ErrorContains(t, pool.loadInfoFromDir(bad, ""), "credentials")
	require.Equal(t, original, pool.sas)
	require.Equal(t, originalPool, pool.saPool)
	require.Equal(t, active, pool.activeIdx)
	require.Error(t, pool.loadInfoFromDir(filepath.Join(bad, "missing"), ""))
	require.Equal(t, original, pool.sas)
}

func TestSARecursiveSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privileges differ on Windows")
	}
	files := testSAFiles(t)
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	require.NoError(t, os.Mkdir(nested, 0700))
	link := filepath.Join(nested, "linked.json")
	require.NoError(t, os.Symlink(files[0], link))
	require.NoError(t, os.Symlink(root, filepath.Join(nested, "cycle")))
	require.NoError(t, os.Symlink(filepath.Dir(files[0]), filepath.Join(root, "external-directory")))
	require.NoError(t, os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "broken.json")))
	var pool SaInfo
	require.NoError(t, pool.loadInfoFromDir(root, link))
	require.Equal(t, map[string]int{link: 0}, pool.saPool)
	rootAlias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(root, rootAlias))
	aliasFile := filepath.Join(rootAlias, "nested", "linked.json")
	require.NoError(t, pool.loadInfoFromDir(rootAlias, aliasFile))
	require.Equal(t, map[string]int{aliasFile: 0}, pool.saPool)
}

func TestSARecursiveStartupAndLiteralPath(t *testing.T) {
	files := testSAFiles(t)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	root := t.TempDir()
	t.Setenv("SA_LITERAL_TEST", "unexpected")
	good := filepath.Join(root, "project_$SA_LITERAL_TEST", "file.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(good), 0700))
	require.NoError(t, os.WriteFile(good, data, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "bad.json"), []byte("bad"), 0600))
	f, err := newFs(context.Background(), "test", "", configmap.Simple{"service_account_file_path": root, "random_pick_sa": "true", "rolling_sa": "true", "rolling_count": "4", "chunk_size": "256Ki"})
	require.NoError(t, err)
	require.Equal(t, good, f.currentService().file)
	require.Equal(t, 4, f.ci.Transfers)
	// Preserve literal filenames when rotating back after another account.
	other := files[0]
	require.NoError(t, f.changeServiceAccountFile(context.Background(), other))
	f.waitChangeSvc.Lock()
	err = f.tryPoolService(context.Background(), good)
	f.waitChangeSvc.Unlock()
	require.NoError(t, err)
	require.Equal(t, good, f.currentService().file)
}

func TestSAQuotaSkipsBrokenCandidate(t *testing.T) {
	files := testSAFiles(t)
	good := filepath.Join(filepath.Dir(files[0]), "c.json")
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(good, data, 0600))
	for i := 0; i < 12; i++ {
		f := testSAFs(t, append(files, good))
		f.opt.ServiceAccountFile = ""
		require.NoError(t, f.changeServiceAccountFile(context.Background(), files[0]))
		require.NoError(t, os.WriteFile(files[1], []byte("bad"), 0600))
		f.changeSvc(context.Background())
		require.Equal(t, good, f.currentService().file)
		require.True(t, f.ServiceAccountFiles.sas[0].isStale)
		f.rollingSvc(context.Background())
		require.Equal(t, good, f.currentService().file)
		require.NotContains(t, f.ServiceAccountFiles.saPool, files[1])
	}
}

func TestSARejectedFileCanRecoverOnRescan(t *testing.T) {
	files := testSAFiles(t)
	data, err := os.ReadFile(files[1])
	require.NoError(t, err)
	f := testSAFs(t, files)
	f.opt.ServiceAccountFile = ""
	require.NoError(t, f.changeServiceAccountFile(context.Background(), files[0]))
	require.NoError(t, os.WriteFile(files[1], []byte("bad"), 0600))
	f.rollingSvc(context.Background())
	require.NotContains(t, f.ServiceAccountFiles.saPool, files[1])
	f.ServiceAccountFiles.revertStaleSa(files[1])
	require.NotContains(t, f.ServiceAccountFiles.saPool, files[1])
	require.NoError(t, os.WriteFile(files[1], data, 0600))
	require.NoError(t, f.ServiceAccountFiles.loadInfoFromDir(filepath.Dir(files[0]), files[0]))
	f.rollingSvc(context.Background())
	require.Equal(t, files[1], f.currentService().file)
}

func TestSAOptionalFieldFailureIsolatedDuringRotation(t *testing.T) {
	files := testSAFiles(t)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	good := filepath.Join(filepath.Dir(files[0]), "c.json")
	require.NoError(t, os.WriteFile(good, data, 0600))
	f := testSAFs(t, append(files, good))
	f.opt.ServiceAccountFile = ""
	require.NoError(t, f.changeServiceAccountFile(context.Background(), files[0]))
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	fields["token_uri"] = json.RawMessage(`false`)
	bad, err := json.Marshal(fields)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(files[1], bad, 0600))
	f.rollingSvc(context.Background())
	require.Equal(t, good, f.currentService().file)
	require.NotContains(t, f.ServiceAccountFiles.saPool, files[1])
	f.rollingSvc(context.Background())
	require.Equal(t, files[0], f.currentService().file)
}

func TestSAExplicitInvalidOptionalFieldPreservesClient(t *testing.T) {
	files := testSAFiles(t)
	f := testSAFs(t, files)
	f.opt.ServiceAccountFile = ""
	require.NoError(t, f.changeServiceAccountFile(context.Background(), files[0]))
	before, credentials := f.currentService(), f.opt.ServiceAccountCredentials
	data, err := os.ReadFile(files[1])
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	const sentinel = "9876543210987654321098765432109876543210"
	fields["service_account_impersonation"] = json.RawMessage(`{"token_lifetime_seconds":` + sentinel + `}`)
	data, err = json.Marshal(fields)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(files[1], data, 0600))
	err = f.changeServiceAccountFile(context.Background(), files[1])
	require.Error(t, err)
	require.NotContains(t, err.Error(), sentinel)
	require.True(t, before == f.currentService())
	require.True(t, credentials == f.opt.ServiceAccountCredentials)
}

func TestSARecursiveBadCredentialsExcluded(t *testing.T) {
	files := testSAFiles(t)
	root := filepath.Dir(files[0])
	bad := filepath.Join(root, "0-invalid.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"type":"service_account","private_key":"SECRET_BAD_KEY"}`), 0600))
	var pool SaInfo
	require.NoError(t, pool.loadInfoFromDir(root, files[0]))
	require.Len(t, pool.saPool, 2)
	require.NotContains(t, pool.saPool, bad)
	for i := 0; i < 20; i++ {
		require.NotEqual(t, bad, pool.sas[pool.randomPick()].saPath)
	}
}

func TestSARecursiveRelativeIdentity(t *testing.T) {
	files := testSAFiles(t)
	cwd, err := os.Getwd()
	require.NoError(t, err)
	relative, err := filepath.Rel(cwd, filepath.Dir(files[0]))
	require.NoError(t, err)
	var pool SaInfo
	require.NoError(t, pool.loadInfoFromDir(relative, files[0]))
	require.Len(t, pool.sas, 2)
	require.Equal(t, files[0], pool.sas[0].saPath)
	require.Equal(t, 0, pool.activeIdx)
	f, err := newFs(context.Background(), "test", "", configmap.Simple{"service_account_file": filepath.Join(relative, "a.json"), "service_account_file_path": relative, "chunk_size": "256Ki"})
	require.NoError(t, err)
	require.Equal(t, files[0], f.currentService().file)
	require.Equal(t, filepath.Dir(files[0]), f.opt.ServiceAccountFilePath)
}

func TestSAEmptyExpandedPathStaysUnspecified(t *testing.T) {
	t.Setenv("GCLONE_EMPTY_SA_PATH", "")
	name, err := serviceAccountPath("$GCLONE_EMPTY_SA_PATH")
	require.NoError(t, err)
	require.Empty(t, name, "empty expansion must not select the working directory")
	_, err = newFs(context.Background(), "test", "", configmap.Simple{
		"service_account_file_path": "$GCLONE_EMPTY_SA_PATH", "random_pick_sa": "true", "chunk_size": "256Ki",
	})
	require.ErrorContains(t, err, "requires service_account_file_path")
}

func TestSARollingSkipsBrokenCandidate(t *testing.T) {
	files := testSAFiles(t)
	good := filepath.Join(filepath.Dir(files[0]), "c.json")
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(good, data, 0600))
	f := testSAFs(t, append(files, good))
	f.opt.ServiceAccountFile = ""
	require.NoError(t, f.changeServiceAccountFile(context.Background(), files[0]))
	require.NoError(t, os.WriteFile(files[1], []byte("broken"), 0600))
	f.rollingSvc(context.Background())
	require.Equal(t, good, f.currentService().file)
	require.NotContains(t, f.ServiceAccountFiles.saPool, files[1])
	require.Contains(t, f.ServiceAccountFiles.saPool, files[0])
	f.rollingSvc(context.Background())
	require.Equal(t, files[0], f.currentService().file)
}

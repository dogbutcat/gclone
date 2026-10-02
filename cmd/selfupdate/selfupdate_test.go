//go:build !noselfupdate
// +build !noselfupdate

package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rclone/rclone/cmd/cmount"
	"github.com/rclone/rclone/lib/buildinfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Check gclone release versions without fetching a release from GitHub.
func TestGetVersion(t *testing.T) {
	for _, test := range []struct {
		name, version, want string
		beta                bool
	}{
		{"release", "v1.75.1-mod1.6.2", "v1.75.1-mod1.6.2", false},
		{"missing prefix", "1.75.1-mod1.6.2", "v1.75.1-mod1.6.2", false},
		{"development", "v1.75.1-DEV", "v1.75.1-DEV", false},
		{"beta unsupported", "1.75.1-mod1.6.2", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, siteURL, err := GetVersion(context.Background(), test.beta, test.version)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
			assert.Equal(t, "https://github.com/dogbutcat/gclone/releases/download/", siteURL)
		})
	}
}

func TestInstallUpdateCheckDoesNotWrite(t *testing.T) {
	target := filepath.Join(t.TempDir(), "gclone")
	err := InstallUpdate(context.Background(), &Options{
		Check: true, Stable: true, Version: "v1.75.1-mod1.6.2", Package: "zip", Output: target,
	})
	_, tags := buildinfo.GetLinkingAndTags()
	if strings.Contains(" "+tags+" ", " cmount ") && !cmount.ProvidedBy(runtime.GOOS) {
		require.EqualError(t, err, "updating would discard the mount FUSE capability, aborting")
	} else {
		require.NoError(t, err)
	}
	_, err = os.Stat(target)
	assert.True(t, os.IsNotExist(err))
}

// Exercise the download extraction and replacement stages using a local archive.
func TestReplaceDownloadedExecutable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "gclone")
	entryName := "gclone-v1.75.1-mod1.6.2/gclone"
	if runtime.GOOS == "windows" {
		target += ".exe"
		entryName += ".exe"
	}
	oldContent := []byte("old executable")
	newContent := []byte("new executable")
	require.NoError(t, os.WriteFile(target, oldContent, 0644))
	require.NoError(t, verifyAccess(target))

	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create(entryName)
	require.NoError(t, err)
	_, err = entry.Write(newContent)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	newFile, err := makeRandomExeName(target, "new")
	require.NoError(t, err)
	require.NoError(t, extractZipToFile(archive.Bytes(), entryName, newFile))
	require.NoError(t, replaceExecutable(target, newFile, target+".old"))
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, newContent, got)
	info, err := os.Stat(target)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0644), info.Mode().Perm())
	}
	_, err = os.Stat(newFile)
	assert.True(t, os.IsNotExist(err))
}

func TestInvalidArchivePreservesExistingFile(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	_, err := writer.Create("different-entry")
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"invalid zip", []byte("invalid zip")},
		{"missing executable", archive.Bytes()},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "gclone")
			original := []byte("existing executable")
			require.NoError(t, os.WriteFile(target, original, 0644))
			require.Error(t, extractZipToFile(test.data, "gclone", target))
			got, err := os.ReadFile(target)
			require.NoError(t, err)
			assert.Equal(t, original, got)
		})
	}
}

func TestIsLatest(t *testing.T) {
	vNow := "v1.63.1-mod1.5.2"
	vNext := "v1.64.0-DEV"
	assert.False(t, isLatest(vNow, vNext))
	vNow = "v1.64.0-DEV"
	vNext = "v1.63.1-mod1.5.2"
	assert.True(t, isLatest(vNow, vNext))
	// local base over next base should true
	vNow = "v1.64.0-mod1.5.2"
	vNext = "v1.63.1-mod1.5.2"
	assert.True(t, isLatest(vNow, vNext))
	// next base over local should false
	vNow = "v1.63.1-mod1.5.2"
	vNext = "v1.64.0-mod1.5.2"
	assert.False(t, isLatest(vNow, vNext))
	// local eq next should true
	vNow = "v1.64.0-mod1.5.2"
	vNext = "v1.64.0-mod1.5.2"
	assert.True(t, isLatest(vNow, vNext))
	// next mod over local should false
	vNow = "v1.63.1-mod1.5.2"
	vNext = "v1.63.1-mod1.5.3"
	assert.False(t, isLatest(vNow, vNext))
	// local mod over next should true
	vNow = "v1.63.1-mod1.5.5"
	vNext = "v1.63.1-mod1.5.3"
	assert.True(t, isLatest(vNow, vNext))
	// next base over local while local mod over next should false
	// logically not possible
	vNow = "v1.63.1-mod1.5.3"
	vNext = "v1.64.0-mod1.5.2"
	assert.False(t, isLatest(vNow, vNext))
	// next base and mod over local should false
	vNow = "v1.63.1-mod1.5.2"
	vNext = "v1.64.1-mod1.5.3"
	assert.False(t, isLatest(vNow, vNext))
	// next mod over local while base is eq should false
	vNow = "v1.63.1-mod1.5.2"
	vNext = "v1.63.1-mod1.5.3"
	assert.False(t, isLatest(vNow, vNext))
}

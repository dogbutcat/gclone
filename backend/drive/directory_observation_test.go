// Check that real listing callbacks and metadata mkdirs share resolver ownership.
package drive

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
	drive "google.golang.org/api/drive/v3"
)

func TestLateListingDoesNotRestoreInvalidatedDirectoryID(t *testing.T) {
	for _, recursive := range []bool{false, true} {
		t.Run(map[bool]string{false: "list", true: "recursive"}[recursive], func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var gate, unblock sync.Once
			defer unblock.Do(func() { close(release) })
			ctx, f, _ := testSAListFixture(t, func(request saListRequest) (int, any) {
				if strings.Contains(request.query.Get("q"), "'root' in parents") {
					gate.Do(func() { close(entered); <-release })
					return 200, &drive.FileList{Files: []*drive.File{{Id: "old-child", Name: "child", MimeType: driveFolderType, Parents: []string{"root"}}}}
				}
				return 200, &drive.FileList{}
			})
			require.NoError(t, f.dirCache.FindRoot(ctx, false))
			done := make(chan error, 1)
			go func() {
				callback := func(fs.DirEntries) error { return nil }
				if recursive {
					done <- f.ListR(ctx, "", callback)
				} else {
					done <- f.ListP(ctx, "", callback)
				}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("root listing not reached")
			}
			f.dirCache.FlushDir("child")
			f.dirCache.Put("child", "new-child")
			unblock.Do(func() { close(release) })
			require.NoError(t, <-done)
			id, ok := f.dirCache.Get("child")
			require.True(t, ok)
			require.Equal(t, "new-child", id)
		})
	}
}

func TestMkdirMetadataJoinsOrdinaryDirectoryCreation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var creates atomic.Int32
	var updated atomic.Bool
	ctx, f, _ := testSAListFixture(t, func(request saListRequest) (int, any) {
		if request.method == "POST" && strings.HasSuffix(request.path, "/files") {
			creates.Add(1)
			close(entered)
			<-release
			return 200, &drive.File{Id: "shared-id", Name: "child", MimeType: driveFolderType}
		}
		if request.method == "PATCH" {
			var info drive.File
			if json.Unmarshal(request.body, &info) == nil && info.Description == "joined-metadata" {
				updated.Store(true)
			}
			return 200, &drive.File{Id: "shared-id", Name: "child", MimeType: driveFolderType, Description: info.Description}
		}
		if strings.Contains(request.path, "/files/shared-id") {
			return 200, &drive.File{Id: "shared-id", Name: "child", MimeType: driveFolderType}
		}
		return 200, &drive.FileList{}
	})
	ordinary := make(chan error, 1)
	go func() { ordinary <- f.Mkdir(ctx, "child") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("create not reached")
	}
	metadata := make(chan error, 1)
	go func() {
		_, err := f.MkdirMetadata(ctx, "child", fs.Metadata{"description": "joined-metadata"})
		metadata <- err
	}()
	once.Do(func() { close(release) })
	require.NoError(t, <-ordinary)
	require.NoError(t, <-metadata)
	require.Equal(t, int32(1), creates.Load())
	require.True(t, updated.Load())
}

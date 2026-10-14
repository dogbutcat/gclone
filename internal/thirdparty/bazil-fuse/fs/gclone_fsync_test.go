// Verify descriptor-specific sync dispatch without holding the server registry.
package fs

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"bazil.org/fuse"
)

type syncNode struct{ calls int }

func (*syncNode) Attr(context.Context, *fuse.Attr) error            { return nil }
func (n *syncNode) Fsync(context.Context, *fuse.FsyncRequest) error { n.calls++; return syscall.ENOSPC }

type syncHandle func() error

func (h syncHandle) Fsync(context.Context, *fuse.FsyncRequest) error { return h() }

func TestGcloneFsyncHandleDispatch(t *testing.T) {
	server := &Server{debug: func(interface{}) {}, handle: make([]*serveHandle, 3)}
	node := new(syncNode)
	server.handle[1] = &serveHandle{handle: syncHandle(func() error {
		// Reentering the registry proves dispatch has released Server.meta.
		if server.getHandle(1) == nil {
			t.Error("handle disappeared")
		}
		return syscall.EIO
	})}
	server.handle[2] = &serveHandle{handle: struct{}{}}
	done := make(chan error, 1)
	go func() { done <- server.fsyncHandle(context.Background(), &fuse.FsyncRequest{Handle: 1}, node) }()
	select {
	case err := <-done:
		if !errors.Is(err, syscall.EIO) || node.calls != 0 {
			t.Fatalf("handle priority/error lost: %v, node calls=%d", err, node.calls)
		}
	case <-time.After(time.Second):
		t.Fatal("sync callback blocked on the server handle registry")
	}
	if err := server.fsyncHandle(context.Background(), &fuse.FsyncRequest{Handle: 2}, node); !errors.Is(err, syscall.ENOSPC) || node.calls != 1 {
		t.Fatalf("node fallback: %v calls=%d", err, node.calls)
	}
	if err := server.fsyncHandle(context.Background(), &fuse.FsyncRequest{Handle: 999}, node); !errors.Is(err, syscall.ESTALE) || node.calls != 1 {
		t.Fatalf("invalid handle: %v calls=%d", err, node.calls)
	}
}

// Verify cache capability negotiation without requiring a mounted filesystem.
package fuse

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestGcloneDataInvalidationNegotiation(t *testing.T) {
	base := InitAsyncRead | InitWritebackCache
	all := base | InitAutoInvalData | InitExplicitInvalidateData
	for _, tc := range []struct {
		name         string
		options      []MountOption
		kernel, want InitFlags
	}{
		{"default unchanged", nil, all, base},
		{"explicit", []MountOption{ExplicitInvalidateData()}, all, base | InitExplicitInvalidateData},
		{"unsupported explicit", []MountOption{ExplicitInvalidateData()}, base, base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
			if err != nil {
				t.Fatal(err)
			}
			conn := &Conn{dev: os.NewFile(uintptr(pair[0]), "fuse-init-fixture")}
			t.Cleanup(func() { unix.Close(pair[1]); conn.Close() })
			if err := unix.SetsockoptTimeval(pair[1], unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 3}); err != nil {
				t.Fatal(err)
			}
			conf := &mountConfig{initFlags: base}
			for _, option := range tc.options {
				if err := option(conf); err != nil {
					t.Fatal(err)
				}
			}
			finished := make(chan error, 1)
			go func() { finished <- initMount(conn, conf) }()
			var order binary.ByteOrder = binary.LittleEndian
			native := uint16(1)
			if *(*byte)(unsafe.Pointer(&native)) == 0 {
				order = binary.BigEndian
			}
			var request bytes.Buffer
			header := inHeader{Len: uint32(inHeaderSize + initInSize), Opcode: opInit, Unique: 1}
			init := initIn{Major: protoVersionMaxMajor, Minor: protoVersionMaxMinor, Flags: uint32(tc.kernel)}
			if err := binary.Write(&request, order, header); err != nil {
				t.Fatal(err)
			}
			if err := binary.Write(&request, order, init); err != nil {
				t.Fatal(err)
			}
			if _, err := unix.Write(pair[1], request.Bytes()); err != nil {
				t.Fatal(err)
			}
			response := make([]byte, 512)
			n, err := unix.Read(pair[1], response)
			if err != nil {
				t.Fatal(err)
			}
			headerSize := binary.Size(outHeader{})
			if n < headerSize+16 {
				t.Fatalf("short INIT response: %d", n)
			}
			flags := InitFlags(order.Uint32(response[headerSize+12 : headerSize+16]))
			if flags != tc.want {
				t.Fatalf("negotiated flags = %v, want %v", flags, tc.want)
			}
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("INIT did not finish")
			}
		})
	}
}

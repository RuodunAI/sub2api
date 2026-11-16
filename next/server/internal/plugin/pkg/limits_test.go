package pkg

import (
	"archive/zip"
	"bytes"
	"io"
	"testing"
)

// Zero limits take the shared plugin package limits: 64 MiB compressed,
// 256 MiB unpacked, 2000 files.
func TestDefaultLimits(t *testing.T) {
	if l := (Limits{}).withDefaults(); l != (Limits{MaxPackageBytes: 64 << 20, MaxUnpackedBytes: 256 << 20, MaxFiles: 2000}) {
		t.Fatalf("zero limits: %+v", l)
	}
}

// zeroZip is a zip with one stored entry of size zero bytes.
func zeroZip(t *testing.T, size int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Grow(int(size) + 1024)
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "bin/plugin", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(w, zeros{}, size); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// The limits are inclusive: exactly the limit passes, one byte more fails.
func TestUnpackLimitBoundaries(t *testing.T) {
	data := zeroZip(t, 4096)
	if _, err := Unpack(data, Limits{MaxPackageBytes: int64(len(data))}); err != nil {
		t.Fatalf("package at the limit: %v", err)
	}
	if _, err := Unpack(data, Limits{MaxPackageBytes: int64(len(data)) - 1}); err == nil {
		t.Fatal("package above the limit accepted")
	}
	if _, err := Unpack(data, Limits{MaxUnpackedBytes: 4096}); err != nil {
		t.Fatalf("unpacked at the limit: %v", err)
	}
	if _, err := Unpack(data, Limits{MaxUnpackedBytes: 4095}); err == nil {
		t.Fatal("unpacked above the limit accepted")
	}
}

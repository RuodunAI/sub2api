//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	execChildEnv = "SUB2API_TEST_EXEC_CHILD"
	execEchoEnv  = "SUB2API_TEST_EXEC_ECHO"
)

// The program TestExecGatewayChild hands off to: a copy of this test binary
// that prints "handed off". /bin/echo cannot stand in for it, because a
// multi-call coreutils refuses to run under another executable name.
func TestExecGatewayEcho(t *testing.T) {
	if os.Getenv(execEchoEnv) == "" {
		t.Skip("run by TestExecGatewayChild")
	}
	fmt.Println("handed off")
	os.Exit(0)
}

// The child of TestExecGatewayRunsTheCheckedDescriptor: a hardened gateway
// process that replaces itself with the binary in execChildEnv.
func TestExecGatewayChild(t *testing.T) {
	spec := os.Getenv(execChildEnv)
	if spec == "" {
		t.Skip("run by TestExecGatewayRunsTheCheckedDescriptor")
	}
	path, digest, _ := strings.Cut(spec, "|")
	if err := hardenProcess(); err != nil {
		t.Fatal(err)
	}
	t.Fatal(execGateway(path, digest, []string{"sub2api-gateway", "-test.run=^TestExecGatewayEcho$"}, []string{execEchoEnv + "=1"}))
}

func TestExecGatewayRunsTheCheckedDescriptor(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "sub2api-gateway")
	if err = os.WriteFile(bin, body, 0755); err != nil {
		t.Fatal(err)
	}
	if err = execGateway(bin, strings.Repeat("0", 64), nil, nil); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("a binary that differs from its digest: %v", err)
	}
	h := sha256.Sum256(body)
	cmd := exec.Command(os.Args[0], "-test.run=^TestExecGatewayChild$")
	cmd.Env = append(os.Environ(), execChildEnv+"="+bin+"|"+hex.EncodeToString(h[:]))
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "handed off" {
		t.Fatalf("exec: %v %q", err, out)
	}
}

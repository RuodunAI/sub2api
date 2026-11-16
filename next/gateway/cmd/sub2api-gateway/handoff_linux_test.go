//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const execChildEnv = "SUB2API_TEST_EXEC_CHILD"

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
	t.Fatal(execGateway(path, digest, []string{"echo", "handed", "off"}, nil))
}

func TestExecGatewayRunsTheCheckedDescriptor(t *testing.T) {
	body, err := os.ReadFile("/bin/echo")
	if err != nil {
		t.Skip("no /bin/echo to exec:", err)
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

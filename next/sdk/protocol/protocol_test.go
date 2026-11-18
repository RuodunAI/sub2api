package protocol

import (
	"testing"

	runtimecontract "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

func TestContractVersions(t *testing.T) {
	if ProtocolVersion != 2 || Handshake.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocol %d, handshake %d", ProtocolVersion, Handshake.ProtocolVersion)
	}
	if got := runtimecontract.HostAPIVersion; HostAPIVersion != 10 || got != 10 {
		t.Fatalf("host api %d, runtime contract %d", HostAPIVersion, got)
	}
}

func TestOutdatedContractMessage(t *testing.T) {
	got := OutdatedContractMessage("demo", "1.2.3", "0.1.304")
	want := "plugin demo@1.2.3 was built for an older plugin protocol and cannot be loaded by this core (0.1.304, plugin protocol 2); install a version built for core 0.1.304 or later"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

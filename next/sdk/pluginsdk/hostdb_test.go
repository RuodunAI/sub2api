package pluginsdk

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
	"github.com/Sub2API-Devs/sup2api/next/sdk/testpg"
)

type dsnHost struct {
	pluginv1.HostServiceClient
	dsn string
}

func (h dsnHost) GetDSN(context.Context, *pluginv1.GetDSNRequest, ...grpc.CallOption) (*pluginv1.GetDSNResponse, error) {
	return &pluginv1.GetDSNResponse{Dsn: h.dsn, Schema: "public"}, nil
}

// A Host.DB handle kept past its context says so (review 1116 F5); one
// asked for again works.
func TestHostDBBoundToContext(t *testing.T) {
	h := newHost(HostInfo{}, dsnHost{dsn: testpg.URL(t)}, nil, false, 1)
	defer h.closeDB()
	ctx, cancel := context.WithCancel(context.Background())
	kept, err := h.DB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var one int
	if err := kept.Raw("SELECT 1").Scan(&one).Error; err != nil || one != 1 {
		t.Fatalf("live handle: %v", err)
	}
	cancel()
	err = kept.Raw("SELECT 1").Scan(&one).Error
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "ask Host.DB again") {
		t.Fatalf("kept handle: %v", err)
	}
	if err := kept.Exec("SELECT 1").Error; !errors.Is(err, context.Canceled) {
		t.Fatalf("kept handle, Exec: %v", err)
	}
	db, err := h.DB(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Raw("SELECT 2").Scan(&one).Error; err != nil || one != 2 {
		t.Fatalf("handle asked again: %v", err)
	}
}

// refusingEgress refuses every tunnel and counts the attempts.
type refusingEgress struct {
	pluginv1.EgressServiceClient
	dials *atomic.Int32
}

func (e refusingEgress) Dial(context.Context, ...grpc.CallOption) (pluginv1.EgressService_DialClient, error) {
	e.dials.Add(1)
	return nil, errors.New("no tunnel here")
}

// The virtual database address goes through the host's tunnel whether or
// not the egress tunnel is installed (strict network mode); any other
// address, a plugin unit test's database, is dialled directly
// (TestHostDBBoundToContext, whose host has no tunnel at all).
func TestHostDBDialsVirtualAddressThroughTunnel(t *testing.T) {
	e := refusingEgress{dials: new(atomic.Int32)}
	dsn := "postgres://plugin@" + protocol.DatabaseAddress + "/plugin?sslmode=disable"
	h := newHost(HostInfo{}, dsnHost{dsn: dsn}, e, false, 1)
	defer h.closeDB()
	db, err := h.DB(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("SELECT 1").Error; err == nil || !strings.Contains(err.Error(), "no tunnel here") {
		t.Fatalf("query: %v", err)
	}
	if e.dials.Load() == 0 {
		t.Fatal("the virtual address was not dialled through the tunnel")
	}
}

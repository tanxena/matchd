package client

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/proto"
)

// The happy path is covered end to end in internal/server. These tests cover
// what a real server never does on purpose: refusing or garbling a login.

// fakeServer accepts one connection, reads the Login, writes reply, and keeps
// the connection open until the test ends.
func fakeServer(t *testing.T, reply []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { // serves exactly one connection
		c, err := ln.Accept()
		if err != nil {
			return
		}
		t.Cleanup(func() { _ = c.Close() })
		if _, _, err := proto.NewReader(c).Next(); err != nil {
			return
		}
		_, _ = c.Write(reply)
	}()
	return ln.Addr().String()
}

func TestDialLoginRejected(t *testing.T) {
	addr := fakeServer(t, proto.AppendReject(nil, &proto.Reject{Reason: proto.ReasonClientInUse}))
	_, err := Dial(addr, 5, time.Second)
	if !errors.Is(err, ErrLoginRejected) || !strings.Contains(err.Error(), "ClientInUse") {
		t.Fatalf("err = %v, want ErrLoginRejected: ClientInUse", err)
	}
}

func TestDialUnexpectedReply(t *testing.T) {
	addr := fakeServer(t, proto.AppendHeartbeat(nil))
	if _, err := Dial(addr, 5, time.Second); err == nil || !strings.Contains(err.Error(), "expected LoginAck") {
		t.Fatalf("err = %v", err)
	}
}

func TestDialTimesOutWithoutReply(t *testing.T) {
	addr := fakeServer(t, nil) // reads the login, never answers
	start := time.Now()
	if _, err := Dial(addr, 5, 200*time.Millisecond); err == nil {
		t.Fatal("Dial succeeded with no LoginAck")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Dial took %v despite a 200ms timeout", d)
	}
}

func TestMsgString(t *testing.T) {
	cases := map[string]Msg{
		"Fill seq=4 match=1 order=3 clordid=3 maker sell 1 @ 105 leaves=1": {Type: proto.TypeFill,
			Fill: proto.Fill{Seq: 4, MatchID: 1, OrderID: 3, ClOrdID: 3, Role: proto.RoleMaker, Side: proto.SideSell, Qty: 1, Price: 105, Leaves: 1}},
		"CancelAck seq=5 order=3 clordid=2 expired buy qty=8 @ 0": {Type: proto.TypeCancelAck,
			CancelAck: proto.CancelAck{Seq: 5, OrderID: 3, ClOrdID: 2, Reason: proto.CancelExpired, Side: proto.SideBuy, Qty: 8}},
		"Reject seq=0 reason=NotLoggedIn clordid=0 order=0 inst=0": {Type: proto.TypeReject,
			Reject: proto.Reject{Reason: proto.ReasonNotLoggedIn}},
	}
	for want, m := range cases {
		if got := m.String(); got != want {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	}
}

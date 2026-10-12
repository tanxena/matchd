package main

import (
	"strings"
	"testing"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/gateway"
	"github.com/tanxena/matchd/internal/server"
)

func TestParseSide(t *testing.T) {
	for in, ok := range map[string]bool{"buy": true, "SELL": true, "b": true, "s": true, "hold": false, "": false} {
		if _, err := parseSide(in); (err == nil) != ok {
			t.Errorf("parseSide(%q) err = %v", in, err)
		}
	}
}

func TestArgumentErrors(t *testing.T) {
	cases := []struct {
		name string
		run  func() error
		want string
	}{
		{"order without client", func() error { return cmdOrder([]string{"--side", "buy", "--qty", "1", "--px", "1"}) }, "--client"},
		{"order bad side", func() error { return cmdOrder([]string{"--client", "1", "--side", "up"}) }, "side"},
		{"order bad type", func() error { return cmdOrder([]string{"--client", "1", "--side", "buy", "--type", "stop"}) }, "type"},
		{"order bad tif", func() error { return cmdOrder([]string{"--client", "1", "--side", "buy", "--tif", "ioc"}) }, "tif"},
		{"cancel without id", func() error { return cmdCancel([]string{"--client", "1"}) }, "--id"},
	}
	for _, c := range cases {
		if err := c.run(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want mention of %q", c.name, err, c.want)
		}
	}
}

// order and cancel against a real in-process server.
func TestOrderAndCancelAgainstServer(t *testing.T) {
	gw := gateway.DefaultConfig()
	gw.Addr = "127.0.0.1:0"
	srv, err := server.Start(server.Config{DataDir: t.TempDir(), Gateway: gw, Instruments: []book.Instrument{{Symbol: "XYZ", TickSize: 1, LotSize: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	addr := srv.Addr()
	if err := cmdOrder([]string{"--addr", addr, "--client", "7", "--side", "sell", "--px", "100", "--qty", "5", "--clordid", "1"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdOrder([]string{"--addr", addr, "--client", "8", "--side", "buy", "--type", "market", "--qty", "2"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdCancel([]string{"--addr", addr, "--client", "7", "--id", "1"}); err != nil {
		t.Fatal(err)
	}
}

// book: subscribe, wait for the snapshot, print, exit.
func TestBookAgainstServer(t *testing.T) {
	gw := gateway.DefaultConfig()
	gw.Addr = "127.0.0.1:0"
	srv, err := server.Start(server.Config{DataDir: t.TempDir(), Gateway: gw, Instruments: []book.Instrument{{Symbol: "XYZ", TickSize: 1, LotSize: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	addr := srv.Addr()
	if err := cmdOrder([]string{"--addr", addr, "--client", "7", "--side", "sell", "--px", "101", "--qty", "5"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdBook([]string{"--addr", addr, "--instrument", "1"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdBook([]string{"--addr", addr, "--instrument", "9"}); err == nil || !strings.Contains(err.Error(), "UnknownInstrument") {
		t.Fatalf("book of an unknown instrument: %v", err)
	}
}

// Command engctl is a small client for the matching engine.
//
//	engctl order   --client 7 --side buy --type limit --px 100 --qty 5
//	engctl cancel  --client 7 --id 12
//	engctl connect --client 7      (interactive; type "help")
//
// Every subcommand takes --addr (default 127.0.0.1:9000) and --instrument
// (default 1). book, subscribe and replay-check arrive with market data
// (milestone 6) and recovery (milestone 5).
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tanxena/matchd/internal/client"
	"github.com/tanxena/matchd/internal/proto"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "order":
		err = cmdOrder(os.Args[2:])
	case "cancel":
		err = cmdCancel(os.Args[2:])
	case "connect":
		err = cmdConnect(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "engctl:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  engctl order   [--addr A] --client N [--instrument I] --side buy|sell --type limit|market [--px P] --qty Q [--tif gtc|day] [--clordid C]
  engctl cancel  [--addr A] --client N [--instrument I] --id ORDER_ID
  engctl connect [--addr A] --client N [--instrument I]`)
	os.Exit(2)
}

// common holds the flags every subcommand shares.
type common struct {
	addr       string
	clientID   uint
	instrument uint
	wait       time.Duration
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.addr, "addr", "127.0.0.1:9000", "engine address")
	fs.UintVar(&c.clientID, "client", 0, "client id to log in as (required, nonzero)")
	fs.UintVar(&c.instrument, "instrument", 1, "instrument id")
	fs.DurationVar(&c.wait, "wait", 300*time.Millisecond, "after sending, print messages until none arrives for this long")
}

func (c *common) dial() (*client.Conn, error) {
	if c.clientID == 0 {
		return nil, errors.New("--client is required and must be nonzero")
	}
	return client.Dial(c.addr, uint32(c.clientID), 5*time.Second)
}

func parseSide(s string) (uint8, error) {
	switch strings.ToLower(s) {
	case "buy", "b":
		return proto.SideBuy, nil
	case "sell", "s":
		return proto.SideSell, nil
	}
	return 0, fmt.Errorf("side %q: want buy or sell", s)
}

func cmdOrder(args []string) error {
	fs := flag.NewFlagSet("order", flag.ExitOnError)
	var c common
	c.register(fs)
	side := fs.String("side", "", "buy or sell")
	typ := fs.String("type", "limit", "limit or market")
	px := fs.Int64("px", 0, "limit price in ticks")
	qty := fs.Int64("qty", 0, "quantity in lots")
	tif := fs.String("tif", "gtc", "gtc or day (limit only)")
	clOrd := fs.Uint64("clordid", 0, "client order id (default: derived from the current time)")
	_ = fs.Parse(args)

	m := proto.NewOrder{Instrument: uint32(c.instrument), Price: *px, Qty: *qty, ClOrdID: *clOrd}
	var err error
	if m.Side, err = parseSide(*side); err != nil {
		return err
	}
	switch strings.ToLower(*typ) {
	case "limit":
		m.Type = proto.TypeLimit
		switch strings.ToLower(*tif) {
		case "gtc":
			m.TIF = proto.TIFGTC
		case "day":
			m.TIF = proto.TIFDay
		default:
			return fmt.Errorf("tif %q: want gtc or day", *tif)
		}
	case "market":
		m.Type, m.Price = proto.TypeMarket, 0
	default:
		return fmt.Errorf("type %q: want limit or market", *typ)
	}
	if m.ClOrdID == 0 {
		// Unique among this client's live orders unless two orders are sent
		// in the same nanosecond. Convenience only; a real client keeps a
		// counter.
		m.ClOrdID = uint64(time.Now().UnixNano())
	}

	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.NewOrder(m); err != nil {
		return err
	}
	return printUntilQuiet(conn, c.wait)
}

func cmdCancel(args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	var c common
	c.register(fs)
	id := fs.Uint64("id", 0, "order id to cancel")
	_ = fs.Parse(args)
	if *id == 0 {
		return errors.New("--id is required")
	}
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Cancel(proto.Cancel{Instrument: uint32(c.instrument), OrderID: *id}); err != nil {
		return err
	}
	return printUntilQuiet(conn, c.wait)
}

// printUntilQuiet prints every message until none arrives for quiet. A
// one-shot order can't know in advance how many fills it will get, so it
// waits for the stream to go quiet instead.
func printUntilQuiet(conn *client.Conn, quiet time.Duration) error {
	for {
		_ = conn.SetReadDeadline(time.Now().Add(quiet))
		m, err := conn.Next()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return nil
			}
			return err
		}
		fmt.Println(m)
	}
}

// cmdConnect is an interactive session: stdin lines become orders and
// cancels, and server messages are printed as they arrive.
func cmdConnect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	fmt.Printf("connected to %s as client %d, instrument %d. Type \"help\".\n", c.addr, c.clientID, c.instrument)

	// Printer goroutine: the only reader of conn. Ends when conn closes.
	readErr := make(chan error, 1)    // carries the reason the printer stopped
	printed := make(chan struct{}, 1) // pinged after each message, for waitQuiet
	go func() {
		for {
			m, err := conn.Next()
			if err != nil {
				readErr <- err
				return
			}
			fmt.Println("<", m)
			select {
			case printed <- struct{}{}:
			default: // a ping is already pending; one is enough
			}
		}
	}()
	// waitQuiet returns once no message has arrived for c.wait, so answers
	// to the last commands are printed before engctl exits.
	waitQuiet := func() {
		for {
			select {
			case <-printed:
			case <-readErr:
				return
			case <-time.After(c.wait):
				return
			}
		}
	}

	inst := uint32(c.instrument)
	var clOrd uint64
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		f := strings.Fields(in.Text())
		if len(f) == 0 {
			continue
		}
		var err error
		switch f[0] {
		case "help":
			fmt.Println(`  buy|sell QTY PX     limit order (gtc)
  buy|sell QTY mkt    market order
  cancel ORDER_ID     cancel an order
  hb                  heartbeat
  quit`)
		case "buy", "sell", "b", "s":
			if len(f) != 3 {
				err = errors.New("usage: buy|sell QTY PX|mkt")
				break
			}
			clOrd++
			m := proto.NewOrder{Instrument: inst, ClOrdID: clOrd, Type: proto.TypeLimit, TIF: proto.TIFGTC}
			m.Side, _ = parseSide(f[0])
			if m.Qty, err = strconv.ParseInt(f[1], 10, 64); err != nil {
				break
			}
			if f[2] == "mkt" {
				m.Type, m.TIF = proto.TypeMarket, 0
			} else if m.Price, err = strconv.ParseInt(f[2], 10, 64); err != nil {
				break
			}
			err = conn.NewOrder(m)
		case "cancel":
			if len(f) != 2 {
				err = errors.New("usage: cancel ORDER_ID")
				break
			}
			var id uint64
			if id, err = strconv.ParseUint(f[1], 10, 64); err == nil {
				err = conn.Cancel(proto.Cancel{Instrument: inst, OrderID: id})
			}
		case "hb":
			err = conn.Heartbeat()
		case "quit", "exit":
			waitQuiet()
			return nil
		default:
			err = fmt.Errorf("unknown command %q (try help)", f[0])
		}
		if err != nil {
			fmt.Println("!", err)
		}
		select {
		case err := <-readErr:
			return fmt.Errorf("connection closed: %w", err)
		default:
		}
	}
	waitQuiet() // stdin ended (e.g. piped input): still show the answers
	return in.Err()
}

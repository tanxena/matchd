// Command engctl is a small client for the matching engine.
//
//	engctl order   --client 7 --side buy --type limit --px 100 --qty 5
//	engctl cancel  --client 7 --id 12
//	engctl connect --client 7      (interactive; type "help")
//
//	engctl book XYZ  [--depth 10]
//	engctl subscribe XYZ
//	engctl instruments
//	engctl add-instrument --symbol XYZ --tick 1 --lot 1
//	engctl replay-check --data ./data   (engine stopped)
//
// Order, cancel, connect, book and subscribe take --addr (the TCP gateway,
// default 127.0.0.1:9000). An instrument is given by symbol or numeric id
// (--instrument, or the argument to book and subscribe); symbols are looked
// up through the HTTP endpoint at --http (default 127.0.0.1:9100), which
// instruments and add-instrument also use (decision 018).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tanxena/matchd/internal/client"
	"github.com/tanxena/matchd/internal/proto"
	"github.com/tanxena/matchd/internal/publisher"
	"github.com/tanxena/matchd/internal/snapshot"
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
	case "replay-check":
		err = cmdReplayCheck(os.Args[2:])
	case "book":
		err = cmdBook(os.Args[2:])
	case "subscribe":
		err = cmdSubscribe(os.Args[2:])
	case "instruments":
		err = cmdInstruments(os.Args[2:])
	case "add-instrument":
		err = cmdAddInstrument(os.Args[2:])
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
  engctl order   [--addr A] --client N [--instrument I] --side buy|sell --type limit|market [--px P] --qty Q [--tif gtc|day|ioc|fok] [--clordid C]
  engctl cancel  [--addr A] --client N [--instrument I] --id ORDER_ID
  engctl connect [--addr A] --client N [--instrument I]
  engctl book      SYMBOL|ID [--addr A] [--http H] [--depth N]
  engctl subscribe SYMBOL|ID [--addr A] [--http H]
  engctl instruments [--http H]
  engctl add-instrument [--http H] --symbol S --tick T --lot L
  engctl replay-check --data DIR

An instrument (I) is a symbol or a numeric id; symbols are resolved via --http.`)
	os.Exit(2)
}

// common holds the flags every subcommand shares.
type common struct {
	addr       string
	httpAddr   string
	clientID   uint
	instrument string
	wait       time.Duration
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.addr, "addr", "127.0.0.1:9000", "engine address")
	fs.StringVar(&c.httpAddr, "http", defaultHTTP, "engine HTTP address, for looking up symbols")
	fs.UintVar(&c.clientID, "client", 0, "client id to log in as (required, nonzero)")
	fs.StringVar(&c.instrument, "instrument", "1", "instrument symbol or id")
	fs.DurationVar(&c.wait, "wait", 300*time.Millisecond, "after sending, print messages until none arrives for this long")
}

const defaultHTTP = "127.0.0.1:9100"

// inst resolves --instrument to an id.
func (c *common) inst() (uint32, error) { return resolveInstrument(c.instrument, c.httpAddr) }

// resolveInstrument turns an instrument argument into an id: a number is an
// id; anything else is a symbol, looked up with GET /instruments.
func resolveInstrument(arg, httpAddr string) (uint32, error) {
	if id, err := strconv.ParseUint(arg, 10, 32); err == nil {
		return uint32(id), nil
	}
	list, err := fetchInstruments(httpAddr)
	if err != nil {
		return 0, fmt.Errorf("looking up %q: %w", arg, err)
	}
	for _, in := range list {
		if in.Symbol == arg {
			return uint32(in.ID), nil
		}
	}
	return 0, fmt.Errorf("no instrument %q (engctl instruments lists them)", arg)
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

func fetchInstruments(httpAddr string) ([]publisher.InstrumentInfo, error) {
	resp, err := httpClient.Get("http://" + httpAddr + "/instruments")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}
	var list []publisher.InstrumentInfo
	return list, json.NewDecoder(resp.Body).Decode(&list)
}

// httpError turns a non-2xx response into an error with the server's
// message.
func httpError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return fmt.Errorf("%s: %s", resp.Status, e.Error)
	}
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
}

// splitTarget takes a leading non-flag argument (book XYZ --depth 5) off
// args, so both the PRD's positional form and --instrument work.
func splitTarget(args []string) (target string, rest []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
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

func parseTIF(s string) (uint8, error) {
	switch strings.ToLower(s) {
	case "gtc":
		return proto.TIFGTC, nil
	case "day":
		return proto.TIFDay, nil
	case "ioc":
		return proto.TIFIOC, nil
	case "fok":
		return proto.TIFFOK, nil
	}
	return 0, fmt.Errorf("tif %q: want gtc, day, ioc or fok", s)
}

func cmdOrder(args []string) error {
	fs := flag.NewFlagSet("order", flag.ExitOnError)
	var c common
	c.register(fs)
	side := fs.String("side", "", "buy or sell")
	typ := fs.String("type", "limit", "limit or market")
	px := fs.Int64("px", 0, "limit price in ticks")
	qty := fs.Int64("qty", 0, "quantity in lots")
	tif := fs.String("tif", "gtc", "gtc, day, ioc or fok (limit orders; a market order with ioc or fok is rejected)")
	clOrd := fs.Uint64("clordid", 0, "client order id (default: derived from the current time)")
	_ = fs.Parse(args)

	m := proto.NewOrder{Price: *px, Qty: *qty, ClOrdID: *clOrd}
	var err error
	if m.Side, err = parseSide(*side); err != nil {
		return err
	}
	switch strings.ToLower(*typ) {
	case "limit":
		m.Type = proto.TypeLimit
		if m.TIF, err = parseTIF(*tif); err != nil {
			return err
		}
	case "market":
		m.Type, m.Price = proto.TypeMarket, 0
		// Passed through only if asked for, so the engine's answer (a
		// reject: IOC and FOK are limit-only) is what gets printed.
		if t := strings.ToLower(*tif); t == "ioc" || t == "fok" {
			m.TIF, _ = parseTIF(t)
		}
	default:
		return fmt.Errorf("type %q: want limit or market", *typ)
	}
	if m.ClOrdID == 0 {
		// Unique among this client's live orders unless two orders are sent
		// in the same nanosecond. Convenience only; a real client keeps a
		// counter.
		m.ClOrdID = uint64(time.Now().UnixNano())
	}
	if m.Instrument, err = c.inst(); err != nil {
		return err
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
	inst, err := c.inst()
	if err != nil {
		return err
	}
	if err := conn.Cancel(proto.Cancel{Instrument: inst, OrderID: *id}); err != nil {
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
	inst, err := c.inst()
	if err != nil {
		return err
	}
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	fmt.Printf("connected to %s as client %d, instrument %d. Type \"help\".\n", c.addr, c.clientID, inst)

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
			fmt.Println(`  buy|sell QTY PX [ioc|fok|day]   limit order (gtc unless given)
  buy|sell QTY mkt    market order
  cancel ORDER_ID     cancel an order
  hb                  heartbeat
  quit`)
		case "buy", "sell", "b", "s":
			if len(f) != 3 && len(f) != 4 {
				err = errors.New("usage: buy|sell QTY PX|mkt [ioc|fok|day]")
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
			if len(f) == 4 {
				if m.TIF, err = parseTIF(f[3]); err != nil {
					break
				}
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

// cmdReplayCheck runs the data directory's WAL through a fresh engine and
// compares it with the newest snapshot and the fill log (PRD 5.4). The
// engine must be stopped: the check reads the files as they are.
func cmdReplayCheck(args []string) error {
	fs := flag.NewFlagSet("replay-check", flag.ExitOnError)
	dir := fs.String("data", "./data", "the engine's data directory")
	_ = fs.Parse(args)
	res, err := snapshot.ReplayCheck(*dir)
	if err != nil {
		return fmt.Errorf("replay-check FAILED: %w", err)
	}
	fmt.Println("replay-check OK:", res.Report)
	if res.WALTornBytes > 0 || res.FillLogTornBytes > 0 {
		fmt.Printf("note: torn tails (WAL %d bytes, fill log %d bytes) will be truncated at the next start\n",
			res.WALTornBytes, res.FillLogTornBytes)
	}
	return nil
}

// marketData connects without logging in and subscribes to an instrument,
// given as "book XYZ" or "book --instrument XYZ" (a symbol or an id).
func marketData(args []string, name string) (*client.Conn, *client.Feed, int, error) {
	target, args := splitTarget(args)
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9000", "engine address")
	httpAddr := fs.String("http", defaultHTTP, "engine HTTP address, for looking up symbols")
	instArg := fs.String("instrument", "1", "instrument symbol or id (or give it as the first argument)")
	depth := fs.Int("depth", 10, "levels per side to print")
	_ = fs.Parse(args)
	if target == "" {
		target = *instArg
	}
	inst, err := resolveInstrument(target, *httpAddr)
	if err != nil {
		return nil, nil, 0, err
	}
	conn, err := client.Dial(*addr, 0, 5*time.Second) // market data needs no login
	if err != nil {
		return nil, nil, 0, err
	}
	feed, err := client.NewFeed(conn, inst)
	if err != nil {
		_ = conn.Close()
		return nil, nil, 0, err
	}
	return conn, feed, *depth, nil
}

// cmdBook prints the top levels from a fresh snapshot (PRD 5.7).
func cmdBook(args []string) error {
	conn, feed, depth, err := marketData(args, "book")
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for !feed.Live() {
		m, err := conn.Next()
		if err != nil {
			return err
		}
		if m.Type == proto.TypeReject {
			return fmt.Errorf("subscribe refused: %v", m.Reject.Reason)
		}
		if err := feed.Apply(m); err != nil {
			return err
		}
	}
	fmt.Print(feed.String(depth))
	return nil
}

// cmdSubscribe streams market data, keeping a book and recovering from gaps
// as it goes; it prints every message and any recovery it performs.
func cmdSubscribe(args []string) error {
	conn, feed, depth, err := marketData(args, "subscribe")
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	printed := false
	for {
		m, err := conn.Next()
		if err != nil {
			return err
		}
		before := feed.Stats
		if err := feed.Apply(m); err != nil {
			return err
		}
		switch m.Type {
		case proto.TypeSnapshot, proto.TypeSnapshotLevel, proto.TypeBookDelta, proto.TypeTrade:
			fmt.Println(m)
		case proto.TypeReject:
			fmt.Println("!", m)
		}
		if feed.Stats.Gaps > before.Gaps {
			fmt.Printf("! gap after md_seq %d: resend requested\n", feed.MDSeq())
		}
		if feed.Stats.Resnapshots > before.Resnapshots {
			fmt.Println("! resend unavailable: subscribing again for a fresh snapshot")
		}
		if feed.Live() && !printed {
			fmt.Print(feed.String(depth))
			printed = true
		}
	}
}

// cmdInstruments lists the instruments (GET /instruments).
func cmdInstruments(args []string) error {
	fs := flag.NewFlagSet("instruments", flag.ExitOnError)
	httpAddr := fs.String("http", defaultHTTP, "engine HTTP address")
	_ = fs.Parse(args)
	list, err := fetchInstruments(*httpAddr)
	if err != nil {
		return err
	}
	fmt.Printf("%4s  %-12s %8s %8s %10s %8s\n", "id", "symbol", "tick", "lot", "md_seq", "orders")
	for _, in := range list {
		fmt.Printf("%4d  %-12s %8d %8d %10d %8d\n", in.ID, in.Symbol, in.TickSize, in.LotSize, in.MDSeq, in.Orders)
	}
	return nil
}

// cmdAddInstrument registers an instrument (POST /instruments). The engine
// logs it like any command, so it survives restarts with the same id.
func cmdAddInstrument(args []string) error {
	fs := flag.NewFlagSet("add-instrument", flag.ExitOnError)
	httpAddr := fs.String("http", defaultHTTP, "engine HTTP address")
	symbol := fs.String("symbol", "", "symbol (required)")
	tick := fs.Int64("tick", 1, "tick size")
	lot := fs.Int64("lot", 1, "lot size")
	_ = fs.Parse(args)
	if *symbol == "" {
		return errors.New("--symbol is required")
	}
	body, _ := json.Marshal(map[string]any{"symbol": *symbol, "tick_size": *tick, "lot_size": *lot})
	resp, err := httpClient.Post("http://"+*httpAddr+"/instruments", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return httpError(resp)
	}
	var in publisher.InstrumentInfo
	if err := json.NewDecoder(resp.Body).Decode(&in); err != nil {
		return err
	}
	fmt.Printf("added %s as instrument %d (tick %d, lot %d)\n", in.Symbol, in.ID, in.TickSize, in.LotSize)
	return nil
}

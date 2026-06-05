// Channel-keyed fanout relay. Domain-agnostic: it forwards opaque frame
// messages from one producer per channel to many subscribers, keeping only
// the latest frame per subscriber (drop-to-latest backpressure).
//
// Two listeners:
//   -ingest    public, token-authenticated (optionally TLS): producers publish
//   -consumer  local: doors subscribe
package main

import (
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
)

const (
	msgHelloProducer = 0x01
	msgHelloConsumer = 0x02
	msgFrame         = 0x10
	maxMsg           = 16 << 20 // RGB frames at terminal resolution are tens of KiB
)

// ---- framing ----------------------------------------------------------------

// readMsg returns the payload and the full on-wire bytes (length prefix + payload).
func readMsg(r io.Reader) (payload, framed []byte, err error) {
	var lenbuf [4]byte
	if _, err = io.ReadFull(r, lenbuf[:]); err != nil {
		return nil, nil, err
	}
	n := binary.BigEndian.Uint32(lenbuf[:])
	if n == 0 || n > maxMsg {
		return nil, nil, fmt.Errorf("bad message length %d", n)
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return nil, nil, err
	}
	framed = append(lenbuf[:], payload...)
	return payload, framed, nil
}

// parseHelloChannel extracts the channel from a consumer hello, or the token
// and channel from a producer hello.
func parseString(b []byte, off int) (string, int, bool) {
	if off >= len(b) {
		return "", 0, false
	}
	n := int(b[off])
	off++
	if off+n > len(b) {
		return "", 0, false
	}
	return string(b[off : off+n]), off + n, true
}

// ---- hub --------------------------------------------------------------------

type subscriber struct {
	mu     sync.Mutex
	latest []byte
	notify chan struct{}
}

func newSubscriber() *subscriber {
	return &subscriber{notify: make(chan struct{}, 1)}
}

// set replaces the pending frame, collapsing any unsent one (drop-to-latest).
func (s *subscriber) set(frame []byte) {
	s.mu.Lock()
	s.latest = frame
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *subscriber) take() []byte {
	<-s.notify
	s.mu.Lock()
	f := s.latest
	s.latest = nil
	s.mu.Unlock()
	return f
}

type channel struct {
	mu       sync.Mutex
	latest   []byte // last frame, sent to new subscribers on join
	subs     map[*subscriber]struct{}
	producer net.Conn // current publisher; a new one kicks the old
}

type hub struct {
	mu       sync.Mutex
	channels map[string]*channel
}

func newHub() *hub { return &hub{channels: map[string]*channel{}} }

func (h *hub) get(name string) *channel {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.channels[name]
	if c == nil {
		c = &channel{subs: map[*subscriber]struct{}{}}
		h.channels[name] = c
	}
	return c
}

func (h *hub) publish(name string, framed []byte) {
	c := h.get(name)
	c.mu.Lock()
	c.latest = framed
	for s := range c.subs {
		s.set(framed)
	}
	c.mu.Unlock()
}

func (h *hub) subscribe(name string, s *subscriber) {
	c := h.get(name)
	c.mu.Lock()
	c.subs[s] = struct{}{}
	if c.latest != nil {
		s.set(c.latest)
	}
	c.mu.Unlock()
}

func (h *hub) unsubscribe(name string, s *subscriber) {
	c := h.get(name)
	c.mu.Lock()
	delete(c.subs, s)
	c.mu.Unlock()
}

// setProducer makes conn the sole publisher for the channel, kicking any
// previous producer so a stale one can't fight the live stream.
func (h *hub) setProducer(name string, conn net.Conn) {
	c := h.get(name)
	c.mu.Lock()
	old := c.producer
	c.producer = conn
	c.mu.Unlock()
	if old != nil && old != conn {
		log.Printf("channel %q: new producer took over; dropping previous", name)
		old.Close() // its read loop errors out and exits
	}
}

func (h *hub) clearProducer(name string, conn net.Conn) {
	c := h.get(name)
	c.mu.Lock()
	if c.producer == conn { // don't clear a newer producer that replaced us
		c.producer = nil
	}
	c.mu.Unlock()
}

// ---- connection handlers ----------------------------------------------------

func (h *hub) handleProducer(conn net.Conn, token string) {
	defer conn.Close()
	payload, _, err := readMsg(conn)
	if err != nil || len(payload) == 0 || payload[0] != msgHelloProducer {
		log.Printf("producer %s: bad hello", conn.RemoteAddr())
		return
	}
	tok, off, ok := parseString(payload, 1)
	if !ok {
		return
	}
	ch, _, ok := parseString(payload, off)
	if !ok {
		return
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(token)) != 1 {
		log.Printf("producer %s: bad token", conn.RemoteAddr())
		return
	}
	log.Printf("producer %s publishing to channel %q", conn.RemoteAddr(), ch)
	h.setProducer(ch, conn)
	defer h.clearProducer(ch, conn)
	for {
		payload, framed, err := readMsg(conn)
		if err != nil {
			log.Printf("producer %s gone: %v", conn.RemoteAddr(), err)
			return
		}
		if payload[0] == msgFrame {
			h.publish(ch, framed)
		}
	}
}

func (h *hub) handleConsumer(conn net.Conn) {
	defer conn.Close()
	payload, _, err := readMsg(conn)
	if err != nil || len(payload) == 0 || payload[0] != msgHelloConsumer {
		return
	}
	ch, _, ok := parseString(payload, 1)
	if !ok {
		return
	}
	log.Printf("consumer %s subscribed to channel %q", conn.RemoteAddr(), ch)

	s := newSubscriber()
	h.subscribe(ch, s)
	defer h.unsubscribe(ch, s)

	// Detect disconnect so we can stop the writer.
	done := make(chan struct{})
	go func() {
		io.Copy(io.Discard, conn)
		close(done)
	}()

	for {
		select {
		case <-done:
			return
		case <-s.notify:
		}
		s.mu.Lock()
		f := s.latest
		s.latest = nil
		s.mu.Unlock()
		if f == nil {
			continue
		}
		if _, err := conn.Write(f); err != nil {
			return
		}
	}
}

func listen(addr string, tlsCfg *tls.Config) (net.Listener, error) {
	if tlsCfg != nil {
		return tls.Listen("tcp", addr, tlsCfg)
	}
	return net.Listen("tcp", addr)
}

// startStackDumper installs a SIGUSR1 handler that prints every goroutine's
// stack to stderr. Run `kill -USR1 <pid>` against a wedged service to see
// which goroutine is stuck where (lock, syscall, channel send/recv).
func startStackDumper() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGUSR1)
	go func() {
		buf := make([]byte, 1<<20)
		for range sigs {
			n := runtime.Stack(buf, true)
			log.Printf("===== SIGUSR1 stack dump (%d goroutines) =====\n%s\n===== end stack dump =====",
				runtime.NumGoroutine(), buf[:n])
		}
	}()
}

func main() {
	ingestAddr := flag.String("ingest", ":7600", "producer ingest listen address")
	consumerAddr := flag.String("consumer", "127.0.0.1:7601", "consumer (door) listen address")
	token := flag.String("token", "", "shared token producers must present")
	certFile := flag.String("tls-cert", "", "TLS cert for the ingest listener")
	keyFile := flag.String("tls-key", "", "TLS key for the ingest listener")
	flag.Parse()

	if *token == "" {
		log.Fatal("a -token is required")
	}

	var ingestTLS *tls.Config
	if *certFile != "" || *keyFile != "" {
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			log.Fatalf("loading TLS keypair: %v", err)
		}
		ingestTLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	h := newHub()

	// SIGUSR1 -> dump every goroutine's stack. Cheap to install, no impact
	// until fired. When the producer reports "STUCK in phase 'send'" the cause
	// is here — `kill -USR1 <pid>` prints which goroutine is wedged where
	// (lock, syscall, channel send). On platforms without SIGUSR1 the signal
	// constant is undefined and the call would refuse, so it's Unix-only;
	// on Windows the no-op is fine because we don't deploy the service there.
	startStackDumper()

	ingestLn, err := listen(*ingestAddr, ingestTLS)
	if err != nil {
		log.Fatalf("ingest listen: %v", err)
	}
	consumerLn, err := listen(*consumerAddr, nil)
	if err != nil {
		log.Fatalf("consumer listen: %v", err)
	}
	log.Printf("ingest on %s (tls=%v), consumers on %s", *ingestAddr, ingestTLS != nil, *consumerAddr)

	go func() {
		for {
			conn, err := ingestLn.Accept()
			if err != nil {
				log.Fatalf("ingest accept: %v", err)
			}
			go h.handleProducer(conn, *token)
		}
	}()

	for {
		conn, err := consumerLn.Accept()
		if err != nil {
			log.Fatalf("consumer accept: %v", err)
		}
		go h.handleConsumer(conn)
	}
}

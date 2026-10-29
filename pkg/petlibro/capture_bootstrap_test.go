package petlibro

import (
	"encoding/binary"
	"encoding/hex"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/tutk"
)

// Unmodified Luffy-encoded device-to-client datagrams from onset_udp.pcap in
// archive plaf203-onset-20261012T131704Z.tar.gz (SHA-256
// 4b50191ddbc7d8ea247b574eee22e3b15121a6fd677cbf203a25017788424a43). They
// are the first reliable messages of two physical PLAF203 startups and carry
// no media, UID or credentials; the only identity is the random per-session
// nonce reproduced below. Decoded, every one has outer header 04 02 1b 0a
// (device IOTC version 0x1b, not the client's 0x1d) and family 0x1000/0x7000
// reliable sequences starting at 0:
//
//	internal 0x40 (8-byte body)       camera-initiated, not a reset reply
//	internal 0x25 id=1 boundary=0x3fff frame=0xffffffff
//	application 0x032b                GETAUDIOOUTFORMAT_RESP
//
// The camera orders its two 0x40 messages and the 0x25 completion by race, so
// the completion was observed at reliable sequence 0 and at sequence 2.
const (
	capturedSessionANonce = "d2f1d00ffaa10ecf" // frames 187446..187461
	capturedSessionBNonce = "5683c1ace16b7e69" // frames 222573..222588
)

var (
	// 0x25 at seq 0, then 0x40 seq 1, 0x40 seq 2, 0x032b seq 3.
	capturedSessionAWire = []string{
		"4e6f4d1d40de60cb3d2c882d80efcada913e872c8e6080cacdbd280c61e536c46e2e8d9c40d040ca2d6d280c40e4cad86e2e8d8c40d040ca2d6d280c80e4cbd8666861726d699a1f968cdf8b",
		"4e6f4d1d40de60cb3d2f882dc0efcada913e872c8e6080cacdbd280c61e536c46e2e8d9c40c040ca2d6d280c40e4cbd86e2e8d8c40d140ca2d6d280cc0e4cbd8206c726168656803",
		"4e6f4d1d40de60cb3d2e882dc0efcada913e872c8e6080cacdbd280c61e536c46e2e8d9c40f040ca2d4d280c40e4cad86e2e8dac40d040ca2d6d280cc0e4cbd8206c726168656d03",
		"4e6f4d1d40de60cb3d24882d40eecadc913e872c8e6080cacdbd280c61e536c46e2e8d9c40e640ca2d0d880c40e4cbd86e2e8dac40d140ca2d6d280c40e5cbd8662e8ded40d130da2dcd3a2c40e4cbd8",
	}
	// 0x40 seq 0, 0x40 seq 1, then 0x25 at seq 2, 0x032b seq 3. This is the
	// 72/72/76-byte pattern in the failing physical trace.
	capturedSessionBWire = []string{
		"4e6fcd0f40d820cb3d2c882dc0efcada90998b8c286180caadac280c2ae506ec6e2e8d9c40d040ca2d6d280c40e4cad86e2e8d8c40d040ca2d6d280cc0e4cbd8206c726168656803",
		"4e6fcd0f40d820cb3d2f882dc0efcada90998b8c286180caadac280c2ae506ec6e2e8d9c40c040ca2d6d280c40e4cbd86e2e8d8c40d140ca2d6d280cc0e4cbd8206c726168656d03",
		"4e6fcd0f40d820cb3d2e882d80efcada90998b8c286180caadac280c2ae506ec6e2e8d9c40f040ca2d4d280c40e4cad86e2e8dac40d040ca2d6d280c80e4cbd8666861726d699a1f968cdf8b",
		"4e6fcd0f40d820cb3d27882d40eecadc90998b8c286180caadac280c2ae506ec6e2e8d9c40e640ca2ded880c40e4cad86e2e8dac40d140ca2d6d280c40e5cbd8662e8ded40d130da2dcd3a2c40e4cbd8",
	}
)

func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type capturedStartup struct {
	name  string
	nonce string
	wire  []string
}

var capturedStartups = []capturedStartup{
	{"completion-first", capturedSessionANonce, capturedSessionAWire},
	{"completion-after-camera-controls", capturedSessionBNonce, capturedSessionBWire},
}

// The sanitized fixtures must remain the real device framing, not a re-encode
// of our own assumptions.
func TestCapturedStartupFixtureLayout(t *testing.T) {
	for _, s := range capturedStartups {
		for i, h := range s.wire {
			p := tutk.ReverseTransCodePartial(nil, mustDecodeHex(t, h))
			if p[0] != 4 || p[1] != 2 || p[2] != deviceMagicVersion || p[2] == magicVersion || p[3] != flagsRecv {
				t.Fatalf("%s[%d] outer=%x", s.name, i, p[:4])
			}
			f, ok := decodeReliable(p[28:])
			if !ok || f.seq != uint16(i) {
				t.Fatalf("%s[%d] reliable=%+v ok=%v", s.name, i, f, ok)
			}
		}
	}
}

// Regression: the device's outer version byte and its first reliable sequence
// (0) must both be accepted, and the correlated 0x25 must complete the reset
// regardless of where the camera's own 0x40 controls land in the sequence.
func TestCapturedResetCompletionInBothObservedOrders(t *testing.T) {
	for _, s := range capturedStartups {
		t.Run(s.name, func(t *testing.T) {
			c := &Client{nonce: mustDecodeHex(t, s.nonce), frames: make(chan *Packet, 8),
				avBuffer: make(map[uint64]*pendingFrag)}
			c.acceptReliableControl(initialReliableRecvACK)
			c.initACKTracking(0xffff)
			c.reset = receiveReset{id: 1, boundary: 0x3fff, pending: true}
			if a, _ := c.nextTransportACK(0, 0); a.ReliableRecvACK != 0xffff {
				t.Fatalf("initial report acknowledges undelivered reliable sequence %d", a.ReliableRecvACK)
			}
			for _, h := range s.wire {
				c.handleEncryptedDatagram(mustDecodeHex(t, h))
			}
			if c.reset.pending || !c.avSequenceAnchored || c.avEpochFloor != 0x3fff {
				t.Fatalf("reset pending=%v anchored=%v floor=%#x", c.reset.pending, c.avSequenceAnchored, c.avEpochFloor)
			}
			if c.reliableRecvACK != 3 || len(c.reliableRX) != 0 {
				t.Fatalf("reliable delivery ack=%d buffered=%d", c.reliableRecvACK, len(c.reliableRX))
			}
		})
	}
}

// A completion for a different reset is still ignored with real framing.
func TestCapturedResetCompletionStillCorrelated(t *testing.T) {
	c := &Client{nonce: mustDecodeHex(t, capturedSessionANonce), frames: make(chan *Packet, 8),
		avBuffer: make(map[uint64]*pendingFrag)}
	c.acceptReliableControl(initialReliableRecvACK)
	c.initACKTracking(0xffff)
	c.reset = receiveReset{id: 2, boundary: 0x3fff, pending: true}
	for _, h := range capturedSessionAWire {
		c.handleEncryptedDatagram(mustDecodeHex(t, h))
	}
	if !c.reset.pending || c.avSequenceAnchored {
		t.Fatal("unmatched completion accepted")
	}
	if c.reliableRecvACK != 3 {
		t.Fatal("reliable delivery must not depend on semantic correlation")
	}
}

// End-to-end startup over UDP: the camera replies to the reset request with
// the captured datagrams and the client must reach IPCAM_START.
func TestCapturedBootstrapReachesStart(t *testing.T) {
	for _, s := range capturedStartups {
		t.Run(s.name, func(t *testing.T) {
			server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			c := &Client{conn: conn, cam: server.LocalAddr().(*net.UDPAddr), nonce: mustDecodeHex(t, s.nonce),
				frames: make(chan *Packet, 64), done: make(chan struct{})}
			var sawStart atomic.Bool
			finished := make(chan struct{})
			defer close(finished)
			go func() {
				buf := make([]byte, 2048)
				replied := false
				for {
					_ = server.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
					n, addr, e := server.ReadFromUDP(buf)
					if e != nil {
						select {
						case <-finished:
							return
						default:
							continue
						}
					}
					plain := tutk.ReverseTransCodePartial(nil, buf[:n])
					if len(plain) < 68 || plain[28] != 12 {
						continue
					}
					f, ok := decodeReliable(plain[28:])
					if !ok {
						continue
					}
					switch op := binary.LittleEndian.Uint32(xorBody(f.body)); {
					case f.family == internalFamily && op == controlReceiveReset && !replied:
						replied = true
						for _, h := range s.wire {
							_, _ = server.WriteToUDP(mustDecodeHex(t, h), addr)
						}
					case f.family == applicationFamily && op == ioctlStart:
						sawStart.Store(true)
					}
				}
			}()
			if err := c.bootstrap(); err != nil {
				t.Fatal(err)
			}
			if !c.mediaStartSent.Load() || !c.avSequenceAnchored || c.avEpochFloor != 0x3fff {
				t.Fatal("bootstrap did not establish the reset epoch before START")
			}
			deadline := time.Now().Add(time.Second)
			for !sawStart.Load() && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if !sawStart.Load() {
				t.Fatal("IPCAM_START not received by camera")
			}
		})
	}
}

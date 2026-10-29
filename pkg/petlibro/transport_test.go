package petlibro

import (
	"bytes"
	"encoding/binary"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/tutk"
)

// Native source-data layout from 3b32c4 / 3be4f8, independent of the
// legacy assembler fixture helpers. No codec inference repairs these fields.
func nativeMedia(seq, total, index uint16, ch, flags byte, frame uint32, data []byte) []byte {
	off := 16
	if flags&8 != 0 {
		off = 24
	}
	in := make([]byte, off+20+len(data))
	in[0], in[1], in[2] = 12, flags, 11
	in[off-5], in[off-4] = 20, 1
	h := in[off:]
	h[0] = ch
	h[1] = flags & 1
	binary.LittleEndian.PutUint16(h[2:], seq)
	binary.LittleEndian.PutUint16(h[4:], total)
	binary.LittleEndian.PutUint16(h[6:], index)
	binary.LittleEndian.PutUint16(h[8:], uint16(len(data)))
	binary.LittleEndian.PutUint32(h[12:], frame)
	copy(h[20:], data)
	fixtureMediaChecksum(in, off)
	return nativeOuter(in)
}
func nativeOuter(in []byte) []byte {
	p := make([]byte, 28+len(in))
	copy(p, []byte{4, 2, magicVersion, flagsRecv})
	binary.LittleEndian.PutUint16(p[4:], uint16(len(in)+12))
	binary.LittleEndian.PutUint16(p[8:], msgSessionD2C)
	copy(p[28:], in)
	return p
}
func nativeReliable(seq uint16, family uint16, body []byte) []byte {
	in := make([]byte, 36+len(body))
	in[0], in[2] = 12, 11
	binary.LittleEndian.PutUint16(in[16:], family)
	binary.LittleEndian.PutUint16(in[18:], seq)
	in[20] = 1
	binary.LittleEndian.PutUint16(in[24:], uint16(len(body)))
	binary.LittleEndian.PutUint32(in[28:], uint32(seq))
	copy(in[36:], body)
	return nativeOuter(in)
}

func TestNativeTransportReceiptIndependentOfCodec(t *testing.T) {
	for _, ch := range []byte{3, 4, 5, 6, 7, 8} {
		for _, flags := range []byte{1, 5, 9, 13} {
			c := newTestClient(0x4007, "hd")
			c.parseDatagram(nativeMedia(0x4007, 1, 0, ch, flags, 1, []byte{0x99, 0x88, 0x77}))
			if c.contiguousAckExt() != 0x4007 || len(drainAll(c)) != 0 {
				t.Fatalf("channel=%d flags=%x receipt/output", ch, flags)
			}
		}
	}
}

func TestNativeMalformedEnvelopeNeverACKs(t *testing.T) {
	base := nativeMedia(0x4007, 1, 0, 5, 1, 1, []byte{1, 2, 3})
	cases := map[string]func([]byte) []byte{
		"wrong session":   func(p []byte) []byte { p[20] = 1; return p },
		"magic":           func(p []byte) []byte { p[0] = 0; return p },
		"outer length":    func(p []byte) []byte { p[4]--; return p },
		"truncated":       func(p []byte) []byte { return p[:len(p)-1] },
		"checksum":        func(p []byte) []byte { p[len(p)-1] ^= 1; return p },
		"parity":          func(p []byte) []byte { p[44] = 12; return p },
		"unknown channel": func(p []byte) []byte { p[44] = 9; return p },
		"zero count":      func(p []byte) []byte { p[48] = 0; return p },
		"bad end length":  func(p []byte) []byte { p[50] = 4; return p },
		"reserved flags":  func(p []byte) []byte { p[29] |= 2; return p },
		"unknown format":  func(p []byte) []byte { p[30] = 99; return p },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(0x4007, "hd")
			p := mutate(append([]byte(nil), base...))
			c.parseDatagram(p)
			if c.contiguousAckExt() != 0x4006 || len(c.avBuffer) != 0 {
				t.Fatal("malformed receipt")
			}
		})
	}
}

func TestNativeUint16FragmentTotals(t *testing.T) {
	for _, total := range []uint16{255, 256, 257} {
		for _, flags := range []byte{0, 8} {
			p := nativeMedia(0x4000, total, total-2, 5, flags, 1, []byte{1})
			m, _, ok := decodeMediaEnvelope(p[28:])
			if !ok || m.totalFrags != total || m.fragIdx != total-2 {
				t.Fatal(total, m, ok)
			}
		}
	}
}

func TestNativeFifteenPositionsArriveOrRemainGenuinelyMissing(t *testing.T) {
	c := newTestClient(0x4007, "hd")
	for seq := uint16(0x4016); seq < 0x4020; seq++ {
		c.parseDatagram(nativeMedia(seq, 1, 0, 7, 1, 1, []byte{1}))
	}
	a, _ := ackFor(t, c)
	want := make([]uint16, 15)
	for n := range want {
		want[n] = uint16(n + 1)
	}
	if a.AVBase != 0x4006 || !reflect.DeepEqual(a.Missing, want) {
		t.Fatal(a)
	}
	for seq := uint16(0x4007); seq <= 0x4015; seq++ {
		// Codec-invalid originals/retries still have valid native envelopes.
		c.parseDatagram(nativeMedia(seq, 1, 0, 8, 9, 1, []byte{0x99}))
	}
	b, _ := ackFor(t, c)
	if len(b.Missing) != 0 || c.contiguousAckExt() != 0x401f {
		t.Fatal(b)
	}
	// The recovered interval is advertised once before advancing report base.
	if b.AVBase != 0x4006 {
		t.Fatal("lost positive recovery interval")
	}
	d, _ := ackFor(t, c)
	if d.AVBase != 0x401f {
		t.Fatal(d)
	}
}

func TestNativeWrapLateDuplicateAndAudioBeforeVideo(t *testing.T) {
	c := newTestClient(0xfffe, "hd")
	for _, seq := range []uint16{0xffff, 0, 0xfffe, 0xffff} {
		c.parseDatagram(nativeMedia(seq, 1, 0, 3, 1, 1, []byte{1}))
	}
	if c.contiguousAckExt() != 0x10000 {
		t.Fatal(c.contiguousAckExt())
	}
	if c.stats.ackDuplicateOrOld.Load() == 0 {
		t.Fatal("duplicate not tracked")
	}
}

func TestNativeUnresolvedGapCannotAliasNextSequenceEpoch(t *testing.T) {
	c := newTestClient(0x4007, "hd")
	c.parseDatagram(nativeMedia(0xc005, 1, 0, 7, 1, 1, []byte{1}))
	c.parseDatagram(nativeMedia(0xc006, 1, 0, 7, 1, 2, []byte{1}))
	if c.ackHighExt != 0xc005 {
		t.Fatal("ambiguous half-space advanced unwrap origin")
	}
	c.parseDatagram(nativeMedia(0x4007, 1, 0, 8, 9, 1, []byte{1}))
	if c.contiguousAckExt() != 0x4007 {
		t.Fatal("late retry lost epoch identity")
	}
}

func TestNativeFragmentedAACAndVariableMetadata(t *testing.T) {
	// One large ADTS frame; declared length 1100, followed by 8 opaque
	// application metadata bytes. Only the final fragment carries that size.
	aac := make([]byte, 1100)
	copy(aac, []byte{0xff, 0xf9, 0x50, 0x80, 0, 0, 0xfc})
	aac[3] |= byte(len(aac) >> 11)
	aac[4] = byte(len(aac) >> 3)
	aac[5] = byte(len(aac)&7)<<5 | 31
	c := newTestClient(0x4000, "hd")
	c.parseDatagram(nativeMedia(0x4001, 2, 8, 4, 13, 1, append(append([]byte(nil), aac[1024:]...), make([]byte, 8)...)))
	if len(drainAll(c)) != 0 {
		t.Fatal("incomplete audio emitted")
	}
	c.parseDatagram(nativeMedia(0x4000, 2, 0, 3, 8, 1, aac[:1024]))
	out := drainAll(c)
	if len(out) != 1 || !bytes.Equal(out[0].Payload, aac) {
		t.Fatal("fragmented audio assembly")
	}
	if c.contiguousAckExt() != 0x4001 {
		t.Fatal("audio fabricated ACK hole")
	}
}

func TestNativeResetCorrelationAndBootstrapEvidence(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	c.avSequenceAnchored = false
	c.acceptReliableControl(1)
	c.reset = receiveReset{id: 7, boundary: 0x3fff, pending: true}
	c.parseDatagram(nativeMedia(0x4002, 1, 0, 3, 1, 3, []byte{1}))
	for seq, body := range [][]byte{
		controlBody(controlReceiveResetDone, 6, 0x3fff, 0xffffffff),
		controlBody(controlReceiveResetDone, 7, 0x4001, 0xffffffff),
		controlBody(controlReceiveResetDone, 7, 0x3fff, 0xffffffff),
	} {
		c.parseDatagram(nativeReliable(uint16(seq+2), internalFamily, body))
	}
	if c.reset.pending || !c.avSequenceAnchored {
		t.Fatal("matching reset not completed")
	}
	a, _ := ackFor(t, c)
	if a.AVBase != 0x3fff || a.AVHigh != 0x4002 || !reflect.DeepEqual(a.Missing, []uint16{1, 2}) {
		t.Fatal(a)
	}
	c.parseDatagram(nativeMedia(0x4000, 1, 0, 4, 9, 1, []byte{1}))
	c.parseDatagram(nativeMedia(0x4001, 1, 0, 3, 1, 2, []byte{1}))
	if c.contiguousAckExt() != 0x4002 {
		t.Fatal("bootstrap receipt continuity")
	}
	// A delayed semantic duplicate cannot reset the live receipt epoch.
	c.parseDatagram(nativeReliable(5, internalFamily, controlBody(controlReceiveResetDone, 7, 0x3fff, 0xffffffff)))
	if c.contiguousAckExt() != 0x4002 || c.reliableRecvACK != 5 {
		t.Fatal("duplicate reset or coupled ACK")
	}
}

func TestNativeClearDiscardsOnlyAuthorizedRange(t *testing.T) {
	c := newTestClient(0x4007, "hd")
	c.acceptReliableControl(1)
	c.parseDatagram(nativeMedia(0x4016, 1, 0, 7, 1, 101, []byte{1}))
	old := c.ackEpoch
	c.parseDatagram(nativeReliable(2, internalFamily, controlBody(controlSenderClear, 3, 0x4015, 100)))
	if c.contiguousAckExt() != 0x4016 || c.reliableRecvACK != 2 || c.ackEpoch == old {
		t.Fatal("sender clear not applied")
	}
	if len(c.reliablePending) != 1 {
		t.Fatal("missing semantic completion")
	}
	for _, p := range c.reliablePending {
		body := xorBody(p.inner[36:])
		if binary.LittleEndian.Uint32(body) != controlSenderCleared {
			t.Fatal("wrong response")
		}
	}
	c.parseDatagram(nativeReliable(3, internalFamily, controlBody(controlSenderClear, 3, 0x4015, 100)))
	if c.contiguousAckExt() != 0x4016 {
		t.Fatal("duplicate clear reset receipt")
	}
	c.parseDatagram(nativeReliable(4, internalFamily, controlBody(controlSenderClear, 2, 0x401f, 102)))
	if c.contiguousAckExt() != 0x4016 {
		t.Fatal("stale ID cleared new range")
	}
	c.parseDatagram(nativeMedia(0x4017, 1, 0, 7, 1, 100, []byte{1}))
	if c.contiguousAckExt() != 0x4016 {
		t.Fatal("old frame crossed explicit discard boundary")
	}
	c.parseDatagram(nativeMedia(0x4017, 1, 0, 7, 1, 102, []byte{1}))
	if c.contiguousAckExt() != 0x4017 {
		t.Fatal("new frame not accepted")
	}
}

func TestNativePeerResetIsOppositeDirection(t *testing.T) {
	c := newTestClient(0x4007, "hd")
	c.acceptReliableControl(1)
	c.parseDatagram(nativeReliable(2, internalFamily, controlBody(controlReceiveReset, 4, 0x8000, 0)))
	if c.contiguousAckExt() != 0x4006 {
		t.Fatal("peer's receive reset changed our receive direction")
	}
	for _, p := range c.reliablePending {
		b := xorBody(p.inner[36:])
		if !bytes.Equal(b, controlBody(controlReceiveResetDone, 4, 0x8000, 0xffffffff)) {
			t.Fatal("wrong empty AV sender response")
		}
	}
}

func TestNativeReliableReorderAndFragmentation(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	c.acceptReliableControl(1)
	p := nativeReliable(3, applicationFamily, []byte{1, 2, 3, 4})
	binary.LittleEndian.PutUint16(p[48:], 2)
	binary.LittleEndian.PutUint16(p[50:], 1)
	binary.LittleEndian.PutUint32(p[56:], 1)
	c.parseDatagram(p)
	if c.reliableRecvACK != 1 {
		t.Fatal("reordered reliable ACK")
	}
	q := nativeReliable(2, applicationFamily, []byte{0, 3, 0, 0})
	binary.LittleEndian.PutUint16(q[48:], 2)
	binary.LittleEndian.PutUint32(q[56:], 1)
	c.parseDatagram(q)
	if c.reliableRecvACK != 3 || len(c.reliableRX) != 0 {
		t.Fatal("reliable reassembly")
	}
	if c.contiguousAckExt() != 0x3fff {
		t.Fatal("reliable sequence contaminated AV")
	}
}

func TestNativeReportCannotCommitAcrossReset(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	c.markACKReceived(0x4000)
	_, old := c.nextTransportACK(0, 0)
	c.resetMedia(0x8000, 0xffffffff, true)
	c.markTransportACKSent(old)
	if c.ackReportedExt != 0x8000 {
		t.Fatal("old ACK committed into new epoch")
	}
}

func TestNativeClearCrossesResetCompletion(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	c.avSequenceAnchored = false
	c.acceptReliableControl(1)
	c.reset = receiveReset{id: 1, boundary: 0x3fff, pending: true}
	c.parseDatagram(nativeReliable(2, internalFamily, controlBody(controlSenderClear, 1, 0x4015, 100)))
	if !c.reset.pending || !c.peerClear.set || len(c.reliablePending) != 1 {
		t.Fatal("clear lost during reset")
	}
	c.parseDatagram(nativeReliable(3, internalFamily, controlBody(controlReceiveResetDone, 1, 0x3fff, 99)))
	if c.reset.pending || c.avEpochFloor != 0x4015 || c.avFrameFloor != 100 {
		t.Fatal("reset erased clear floor")
	}
	c.parseDatagram(nativeReliable(4, internalFamily, controlBody(controlSenderClear, 2, 0x4016, 90)))
	if c.avFrameFloor != 100 {
		t.Fatal("new clear ID regressed frame floor")
	}
}

func TestNativeLargeFramesAssembleWithUint16Counts(t *testing.T) {
	for _, count := range []uint16{255, 256, 257} {
		for _, timed := range []byte{0, 8} {
			c := newTestClient(0x4000, "hd")
			var expected []byte
			for i := uint16(0); i < count-1; i++ {
				data := make([]byte, 1024)
				if i == 0 {
					copy(data, []byte{0, 0, 0, 1, 0x65, 0x80})
				}
				expected = append(expected, data...)
				c.parseDatagram(nativeMedia(0x4000+i, count, i, 6, timed, 1, data))
			}
			end := makeTrailerPayload([]byte{0x80}, true, 1, 1000)
			expected = append(expected, 0x80)
			c.parseDatagram(nativeMedia(0x4000+count-1, count, 16, 6, timed|1, 1, end))
			out := drainAll(c)
			if len(out) != 1 || !bytes.Equal(out[0].Payload, expected) || c.contiguousAckExt() != uint64(0x4000+count-1) {
				t.Fatalf("count=%d timed=%d", count, timed)
			}
		}
	}
}

func TestNativeTrailerLikeDataIsNotAnEnd(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	data := makeTrailerPayload([]byte{0, 0, 0, 1, 0x65, 0x88}, true, 1, 1000)
	c.parseDatagram(nativeMedia(0x4000, 2, 0, 5, 8, 1, data))
	if len(drainAll(c)) != 0 {
		t.Fatal("data suffix terminated AU")
	}
	end := makeTrailerPayload([]byte{0x80}, true, 1, 1040)
	c.parseDatagram(nativeMedia(0x4001, 2, 16, 5, 9, 1, end))
	out := drainAll(c)
	if len(out) != 1 || !bytes.Equal(out[0].Payload, append(data, 0x80)) {
		t.Fatal("data suffix was stripped")
	}
}

func TestNativeReportGateAndFailedWrite(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	now := time.Unix(100, 0)
	c.ackLastSent = now
	c.markACKReceived(0x4001)
	if c.reportDue(now.Add(25*time.Millisecond)) || !c.reportDue(now.Add(100*time.Millisecond)) {
		t.Fatal("native feedback gate")
	}
	c.ackForce = true
	if !c.reportDue(now) {
		t.Fatal("reliable delivery must force report")
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	c.conn = conn
	c.cam = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	if c.sendReceiveReport(now.Add(time.Second)) == nil {
		t.Fatal("closed socket write succeeded")
	}
	if c.ackReportedExt != 0x3fff || !c.ackForce || !c.ackLastSent.Equal(now) {
		t.Fatal("failed report committed")
	}
}

func TestNativeMissingResetDoesNotInventEpoch(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &Client{conn: conn, cam: server.LocalAddr().(*net.UDPAddr), nonce: make([]byte, 8), done: make(chan struct{})}
	c.acceptReliableControl(1)
	c.initACKTracking(0xffff)
	c.reset = receiveReset{id: 1, boundary: 0x3fff, pending: true}
	if err := c.pumpBootstrap(time.Now().Add(25*time.Millisecond), true); err == nil {
		t.Fatal("missing reset accepted")
	}
	if c.avSequenceAnchored || !c.reset.pending {
		t.Fatal("timeout invented an AV epoch")
	}
}

func TestNativeControlBoundsAndWrongFamily(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	c.acceptReliableControl(1)
	c.reset = receiveReset{id: 1, boundary: 0x8000, pending: true}
	c.parseDatagram(nativeReliable(2, applicationFamily, controlBody(controlReceiveResetDone, 1, 0x8000, 0xffffffff)))
	c.parseDatagram(nativeReliable(3, internalFamily, controlBody(controlReceiveResetDone, 1, 0x8000, 0xffffffff)[:8]))
	if !c.reset.pending || c.avEpochFloor != 0x3fff || c.reliableRecvACK != 3 {
		t.Fatal("application/malformed semantic body changed epoch")
	}
	// A conflicting sequence cannot replace an already owned deferred fragment.
	p := nativeReliable(5, internalFamily, controlBody(controlReceiveResetDone, 99, 0x8000, 0xffffffff))
	c.parseDatagram(p)
	q := nativeReliable(5, internalFamily, controlBody(controlReceiveResetDone, 1, 0x8000, 0xffffffff))
	c.parseDatagram(q)
	c.parseDatagram(nativeReliable(4, applicationFamily, []byte{0, 3, 0, 0}))
	if !c.reset.pending {
		t.Fatal("duplicate replaced owned reliable data")
	}
	for n := uint16(10); n < 400; n++ {
		c.receiveReliable(reliableFragment{seq: n, total: 1, body: []byte{1}, family: applicationFamily})
	}
	if len(c.reliableRX) > 256 || c.reliableBytes > maxReliableBytes {
		t.Fatal("unbounded reliable receive")
	}
}

func TestNativeReliableRetryAndACKIndependence(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := newTestClient(0x4000, "hd")
	c.conn = conn
	c.cam = server.LocalAddr().(*net.UDPAddr)
	if err := c.sendReliable(internalFamily, controlBody(controlReceiveReset, 1, 0x3fff, 0)); err != nil {
		t.Fatal(err)
	}
	p := c.reliablePending[0]
	first := append([]byte(nil), p.inner...)
	if err := c.retryReliable(p.last.Add(defaultACKInterval)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	for i := 0; i < 2; i++ {
		_ = server.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := server.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		plain := tutk.ReverseTransCodePartial(nil, buf[:n])
		if !bytes.Equal(plain[28:], first) {
			t.Fatal("retry changed request identity")
		}
	}
	c.acceptReliableACK(100)
	if len(c.reliablePending) != 1 {
		t.Fatal("unsent future ACK retired request")
	}
	c.acceptReliableACK(0)
	if len(c.reliablePending) != 0 || c.contiguousAckExt() != 0x3fff {
		t.Fatal("reliable/AV coupling")
	}
}

func TestNativeClosedSessionDoesNotAcceptMoreData(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	c.done = make(chan struct{})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c.parseDatagram(nativeMedia(0x4000, 1, 0, 3, 1, 1, []byte{1}))
	if c.contiguousAckExt() != 0x3fff {
		t.Fatal("closed session mutated")
	}
	next := newTestClient(0x4000, "hd")
	if next.ackHighExt != 0x3fff || next.reliableRecvSet {
		t.Fatal("session inherited old state")
	}
}

func FuzzNativeEnvelopeBounds(f *testing.F) {
	f.Add(nativeMedia(0x4000, 1, 0, 5, 1, 1, []byte{1}))
	f.Add(nativeMedia(0xffff, 257, 256, 8, 8, 10, []byte{1, 2, 3}))
	f.Fuzz(func(t *testing.T, p []byte) {
		c := newTestClient(0x4000, "hd")
		c.parseDatagram(p)
		if len(p) > 28 {
			m, _, ok := decodeMediaEnvelope(p[28:])
			if ok && (len(m.payload) > 1024 || m.totalFrags == 0 || m.totalFrags > maxMediaFragments) {
				t.Fatal("envelope invariant")
			}
		}
	})
}

func TestNativeBootstrapOverLoopback(t *testing.T) {
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
	c := &Client{conn: conn, cam: server.LocalAddr().(*net.UDPAddr), nonce: make([]byte, 8), frames: make(chan *Packet, 64), done: make(chan struct{})}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		buf := make([]byte, 2048)
		replySeq := uint16(2)
		dropped := false
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
			if len(plain) < 68 {
				continue
			}
			in := plain[28:]
			if in[0] != 12 {
				continue
			}
			f, ok := decodeReliable(in)
			if !ok {
				continue
			}
			body := xorBody(f.body)
			if f.family == internalFamily && binary.LittleEndian.Uint32(body) == controlReceiveReset {
				if !dropped {
					dropped = true
					continue
				} // force the real reliable retry path
				resp := nativeReliable(replySeq, internalFamily, controlBody(controlReceiveResetDone, 1, 0x3fff, 0xffffffff))
				replySeq++
				_, _ = server.WriteToUDP(tutk.TransCodePartial(nil, resp), addr)
			} else if binary.LittleEndian.Uint32(body) == ioctlStart {
				for _, seq := range []uint16{0x4000, 0x4002} {
					p := nativeMedia(seq, 1, 0, 7, 1, uint32(seq), []byte{1})
					_, _ = server.WriteToUDP(tutk.TransCodePartial(nil, p), addr)
				}
			}
		}
	}()
	if err := c.bootstrap(); err != nil {
		t.Fatal(err)
	}
	if c.contiguousAckExt() != 0x4000 {
		t.Fatal("bootstrap positively acknowledged missing 4001")
	}
	a, _ := ackFor(t, c)
	if !reflect.DeepEqual(a.Missing, []uint16{1}) {
		t.Fatal(a)
	}
}

package petlibro

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"reflect"
	"testing"
)

func TestCompressFragmentRanges(t *testing.T) {
	asm := &channelAsm{curAUTotal: 8, receivedData: map[uint16]struct{}{0: {}, 2: {}, 6: {}}}
	missing, received := missingFragmentRanges(asm, true)
	if got, want := compressFragmentRanges(missing), "1,3-5"; got != want {
		t.Fatalf("missing ranges=%q, want %q", got, want)
	}
	if received != 4 { // three data fragments plus trailer
		t.Fatalf("received=%d, want 4", received)
	}
}

// ---------- wrapSeq.extend wrap-around ---------------------------------

// TestWrapSeqExtendForwardSmall — simple forward step within the same
// 16-bit window, no wrap involved. Baseline sanity.
func TestWrapSeqExtendForwardSmall(t *testing.T) {
	w := wrapSeq{ext: 0x4000}
	got, ok := w.extend(0x4001)
	if !ok {
		t.Fatalf("extend(0x4001) returned ok=false")
	}
	if got != 0x4001 {
		t.Fatalf("extend(0x4001) from ext=0x4000 = %d, want 0x4001", got)
	}
}

// TestWrapSeqExtendThroughUint16Boundary — the camera's 16-bit AV-seq
// counter wraps every ~65k fragments (a few seconds at HD bitrates).
// extend() must keep the 64-bit projection monotonic across the
// 0xFFFF -> 0x0000 boundary.
func TestWrapSeqExtendThroughUint16Boundary(t *testing.T) {
	w := wrapSeq{ext: 0xFFFF}
	got, ok := w.extend(0x0000)
	if !ok {
		t.Fatalf("extend(0x0000) returned ok=false")
	}
	if got != 0x10000 {
		t.Fatalf("extend(0x0000) from ext=0xFFFF = 0x%x, want 0x10000", got)
	}
	w.advanceTo(0x0000)
	if w.ext != 0x10000 {
		t.Fatalf("advanceTo(0x0000) left ext=0x%x, want 0x10000", w.ext)
	}
	// Another small step after the wrap stays monotonic.
	got, ok = w.extend(0x0005)
	if !ok {
		t.Fatalf("post-wrap extend(0x0005) returned ok=false")
	}
	if got != 0x10005 {
		t.Fatalf("post-wrap extend(0x0005) = 0x%x, want 0x10005", got)
	}
}

// TestWrapSeqExtendFarBackwardLouddrop — P1-3 turned the "silently
// saturate to 0" behaviour into an explicit ok=false return so the
// caller can drop the fragment.  To take the back branch, fwd =
// (wire - lastWire) & 0xFFFF must be >= 0x8000.  With ext=5
// (lastWire=5), wire=0x8005 yields fwd = 0x8000 → back-distance =
// 0x8000 > 5, returns ok=false.
func TestWrapSeqExtendFarBackwardLouddrop(t *testing.T) {
	w := wrapSeq{ext: 5}
	_, ok := w.extend(0x8005)
	if ok {
		t.Fatalf("far-back extend should return ok=false, got ok=true")
	}
}

// TestWrapSeqExtendNearBackwardWorks — back-distance smaller than ext
// is honoured: a late-arriving fragment from "just before" the current
// position resolves to the correct earlier ext value.
func TestWrapSeqExtendNearBackwardWorks(t *testing.T) {
	w := wrapSeq{ext: 0x10010}
	// lastWire = 0x10 (low 16 of 0x10010). wire=0x000F → fwd =
	// (0xF-0x10)&0xFFFF = 0xFFFF → back path, back-distance=1, fits.
	got, ok := w.extend(0x000F)
	if !ok {
		t.Fatalf("near-back extend(0x000F) returned ok=false")
	}
	if got != 0x1000F {
		t.Fatalf("near-back extend(0x000F) from ext=0x10010 = 0x%x, want 0x1000F", got)
	}
}

// TestWrapSeqAdvanceToNoRegress — advanceTo must never lower ext.
func TestWrapSeqAdvanceToNoRegress(t *testing.T) {
	w := wrapSeq{ext: 0x12345}
	w.advanceTo(0x2345) // same low-16 as ext, no movement
	if w.ext != 0x12345 {
		t.Fatalf("advanceTo same-low-16 changed ext to 0x%x, want 0x12345", w.ext)
	}
	// A small forward step.
	w.advanceTo(0x2346)
	if w.ext != 0x12346 {
		t.Fatalf("advanceTo +1 left ext=0x%x, want 0x12346", w.ext)
	}
	// A look-back attempt must not regress.
	w.advanceTo(0x2340)
	if w.ext != 0x12346 {
		t.Fatalf("backward advanceTo regressed ext to 0x%x, want 0x12346", w.ext)
	}
}

// ---------- stripFragmentMetadataTrailer positive + negative -----------

// makeTrailerPayload constructs a synthetic end-fragment payload: a
// few "slice" bytes followed by the 16-byte metadata trailer the
// Petlibro firmware appends. Layout per stripFragmentMetadataTrailer's
// docstring:
//
//	<slice bytes ...> <codec_id 1B = 0x4e> <00 b1 00 onlineNum> <00*7> <ts:4 LE>
func makeTrailerPayload(slice []byte, isIDR bool, onlineNum byte, ts uint32) []byte {
	out := append([]byte{}, slice...)
	out = append(out, CodecH264) // codec_id 0x4e
	b1 := byte(0x00)             // P-frame
	if isIDR {
		b1 = 0x01
	}
	out = append(out, 0x00, b1, 0x00, onlineNum)
	out = append(out, 0, 0, 0, 0, 0, 0, 0) // 7 zero bytes
	var tsBytes [4]byte
	binary.LittleEndian.PutUint32(tsBytes[:], ts)
	out = append(out, tsBytes[:]...)
	return out
}

// TestStripFragmentMetadataTrailerPositive — a synthetic end-fragment
// with a valid signature returns stripped bytes, ts, and onlineNum.
func TestStripFragmentMetadataTrailerPositive(t *testing.T) {
	slice := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE}
	p := makeTrailerPayload(slice, true, 0x01, 0xDEADBEEF)

	stripped, ts, sid, ok := stripFragmentMetadataTrailer(p)
	if !ok {
		t.Fatalf("hasTs=false on a valid trailer; len(p)=%d", len(p))
	}
	if string(stripped) != string(slice) {
		t.Fatalf("stripped=%x, want %x", stripped, slice)
	}
	if ts != 0xDEADBEEF {
		t.Fatalf("ts=0x%x, want 0xDEADBEEF", ts)
	}
	if sid != 0x01 {
		t.Fatalf("onlineNum=0x%02x, want 0x01", sid)
	}
}

// TestStripFragmentMetadataTrailerOnlineNumTwo checks another valid
// online-client count.
func TestStripFragmentMetadataTrailerOnlineNumTwo(t *testing.T) {
	slice := []byte{0x01, 0x02, 0x03}
	p := makeTrailerPayload(slice, false, 0x02, 0x12345678)

	_, ts, sid, ok := stripFragmentMetadataTrailer(p)
	if !ok {
		t.Fatalf("hasTs=false")
	}
	if sid != 0x02 {
		t.Fatalf("onlineNum=0x%02x, want 0x02", sid)
	}
	if ts != 0x12345678 {
		t.Fatalf("ts=0x%x, want 0x12345678", ts)
	}
}

// TestStripFragmentMetadataTrailerNegativeWrongCodec — the 16-byte
// trailer-signature check must reject a payload whose [-16] byte
// isn't 0x4e (the H.264 codec_id). This is the discriminator that
// keeps mid-frame data fragments out of the strip path.
func TestStripFragmentMetadataTrailerNegativeWrongCodec(t *testing.T) {
	slice := []byte{0xAA, 0xBB}
	p := makeTrailerPayload(slice, false, 0x01, 1)
	// Corrupt the codec_id byte (which lives at p[len(p)-16]).
	p[len(p)-16] = 0x42

	stripped, ts, sid, ok := stripFragmentMetadataTrailer(p)
	if ok {
		t.Fatalf("hasTs=true on a wrong-codec trailer; should not match")
	}
	if &stripped[0] != &p[0] {
		t.Fatalf("on negative match, stripped should alias p (not copy)")
	}
	if ts != 0 || sid != 0 {
		t.Fatalf("on negative match, ts/sid must be zero; got ts=0x%x sid=0x%02x", ts, sid)
	}
}

// TestStripFragmentMetadataTrailerNegativeNonZeroFill — a payload that
// has the right codec_id but corrupt zero-fill region must NOT match.
// This guards against accidental real-slice matches: 84 fixed bits of
// zero are what make the signature 1-in-2^84.
func TestStripFragmentMetadataTrailerNegativeNonZeroFill(t *testing.T) {
	slice := []byte{0xAA, 0xBB}
	p := makeTrailerPayload(slice, true, 0x01, 1)
	// Corrupt one of the 7 zero bytes (positions [len(p)-11 .. len(p)-5]).
	p[len(p)-8] = 0x42

	_, _, _, ok := stripFragmentMetadataTrailer(p)
	if ok {
		t.Fatalf("hasTs=true on a corrupted-zero-fill trailer; signature too lax")
	}
}

// TestStripFragmentMetadataTrailerTooShort — payloads shorter than 16
// bytes can't possibly carry the trailer.
func TestStripFragmentMetadataTrailerTooShort(t *testing.T) {
	for _, n := range []int{0, 1, 15} {
		p := make([]byte, n)
		_, _, _, ok := stripFragmentMetadataTrailer(p)
		if ok {
			t.Errorf("hasTs=true for %d-byte payload; expected false", n)
		}
	}
}

// FRAMEINFO byte 4 is onlineNum, so it is not restricted to the values 1/2
// seen in early captures and does not participate in trailer recognition.
func TestStripFragmentMetadataTrailerOnlineNumRange(t *testing.T) {
	for _, onlineNum := range []byte{0, 1, 2, 3, 0x0f, 0x10, 0xff} {
		p := makeTrailerPayload([]byte{0x99}, true, onlineNum, 0)
		_, _, _, ok := stripFragmentMetadataTrailer(p)
		if !ok {
			t.Errorf("onlineNum=0x%02x was rejected", onlineNum)
		}
	}
}

// ---------- frame reassembly end-to-end --------------------------------

// fragSpec describes one synthetic wire fragment we'll feed into
// parseDatagram. Encoding mirrors what client.go's switch arms in
// parseDatagram read from `inner`.
type fragSpec struct {
	channel    byte   // inner[16]
	sub17      byte   // inner[17]
	b1         byte   // inner[1]
	subWire    uint16 // inner[18:20]: AV-sequence (the wrap-counter)
	totalFrags uint16 // inner[20:22]
	fragIdx    uint16 // inner[22:24]
	paylen     uint16 // inner[24:26]: real payload length (0 = "rest of buf")
	frameNum   uint32 // inner[28:32]: camera's frame counter for this channel
	payload    []byte // bytes from inner[36:]
}

type extendedFragSpec struct {
	b1           byte
	channel      byte
	subFlag      byte
	subWire      uint16
	totalFrags   uint16
	fragIdx      uint16
	frameNum     uint32
	mediaFrameNo uint32
	payload      []byte
	extra        []byte
}

// makePkt synthesises the post-decryption packet bytes parseDatagram
// expects: a 0x1C-byte outer header followed by an inner of length
// 36 + len(payload).  Only the offsets actually read are populated.
func makePkt(f fragSpec) []byte {
	inner := make([]byte, 36+len(f.payload))
	inner[0] = 0x0c
	inner[1] = f.b1
	inner[2] = 11
	if _, _, _, trailer := stripFragmentMetadataTrailer(f.payload); trailer {
		inner[1] |= 1
		f.sub17 = 1
		f.fragIdx = 16
	} else if f.totalFrags == 1 && f.b1&^byte(13) == 0 && f.sub17 == 0 {
		inner[1] |= 1
		f.sub17 = 1
		f.fragIdx = 0
	}
	inner[16] = f.channel
	inner[17] = f.sub17
	binary.LittleEndian.PutUint16(inner[18:], f.subWire)
	binary.LittleEndian.PutUint16(inner[20:], f.totalFrags)
	binary.LittleEndian.PutUint16(inner[22:], f.fragIdx)
	binary.LittleEndian.PutUint16(inner[24:], f.paylen)
	binary.LittleEndian.PutUint32(inner[28:], f.frameNum)
	copy(inner[36:], f.payload)
	fixtureMediaChecksum(inner, 16)

	pkt := make([]byte, 0x1C+len(inner))
	copy(pkt[:3], []byte{4, 2, magicVersion})
	pkt[3] = flagsRecv
	binary.LittleEndian.PutUint16(pkt[4:], uint16(0x0c+len(inner)))
	binary.LittleEndian.PutUint16(pkt[8:], msgSessionD2C)
	copy(pkt[0x1C:], inner)
	return pkt
}

// makeCapturedNormalPkt preserves a capture-derived 36-byte normal-media
// header while allowing the media payload to be sanitized. Raw camera media
// is intentionally not retained in the repository.
func makeCapturedNormalPkt(t *testing.T, headerHex string, payload []byte) []byte {
	t.Helper()
	header, err := hex.DecodeString(headerHex)
	if err != nil {
		t.Fatal(err)
	}
	if len(header) != 36 {
		t.Fatalf("capture header is %d bytes, want 36", len(header))
	}
	if got := int(binary.LittleEndian.Uint16(header[24:26])); got != len(payload) {
		t.Fatalf("capture header payload length=%d, fixture payload=%d", got, len(payload))
	}
	inner := append(header, payload...)
	fixtureMediaChecksum(inner, 16) // sanitized payload requires a new checksum
	pkt := make([]byte, 0x1c+len(inner))
	copy(pkt[:3], []byte{4, 2, magicVersion})
	pkt[3] = flagsRecv
	binary.LittleEndian.PutUint16(pkt[4:], uint16(0x0c+len(inner)))
	binary.LittleEndian.PutUint16(pkt[8:], msgSessionD2C)
	copy(pkt[0x1c:], inner)
	return pkt
}

// These headers were independently decrypted from onset_udp.pcap in archive
// plaf203-onset-20261012T131704Z.tar.gz (SHA-256
// 4b50191ddbc7d8ea247b574eee22e3b15121a6fd677cbf203a25017788424a43).
// PCAP frame 177683 is sequence 33744, retransmit-audio channel 0x04;
// frame 177684 is sequence 33745, retransmit-inter channel 0x08. Payloads are
// replaced below with deterministic media-shaped bytes for privacy.
const (
	capturedRetransAudio33744Header = "0c010b00feba050097cf0519010000000401d0830100100098008f452bba2600cc801800"
	capturedRetransInter33745Header = "0c000b00ffba050097cf0619010000000800d183070000000004ed2a2cba260061390e00"
)

func capturedRetransAudio33744(t *testing.T) []byte {
	payload := make([]byte, 152)
	copy(payload, []byte{0xff, 0xf1, 0x50, 0x40, 0x11, 0x01, 0x24})
	return makeCapturedNormalPkt(t, capturedRetransAudio33744Header, payload)
}

func capturedRetransInter33745(t *testing.T) []byte {
	payload := dataFragPayload(0x31, 1024)
	copy(payload, []byte{0x00, 0x00, 0x00, 0x01, 0x61})
	return makeCapturedNormalPkt(t, capturedRetransInter33745Header, payload)
}

func makeExtendedPkt(f extendedFragSpec) []byte {
	inner := make([]byte, 44+len(f.payload)+len(f.extra))
	inner[0] = 0x0c
	inner[1] = f.b1
	inner[2] = 11
	inner[24] = f.channel
	inner[25] = f.subFlag
	binary.LittleEndian.PutUint16(inner[26:28], f.subWire)
	binary.LittleEndian.PutUint16(inner[28:30], f.totalFrags)
	binary.LittleEndian.PutUint16(inner[30:32], f.fragIdx)
	binary.LittleEndian.PutUint16(inner[32:34], uint16(len(f.payload)))
	binary.LittleEndian.PutUint32(inner[36:40], f.frameNum)
	binary.LittleEndian.PutUint32(inner[40:44], f.mediaFrameNo)
	copy(inner[44:], f.payload)
	copy(inner[44+len(f.payload):], f.extra)
	fixtureMediaChecksum(inner, 24)

	pkt := make([]byte, 0x1C+len(inner))
	copy(pkt[:3], []byte{4, 2, magicVersion})
	pkt[3] = flagsRecv
	binary.LittleEndian.PutUint16(pkt[4:], uint16(0x0c+len(inner)))
	binary.LittleEndian.PutUint16(pkt[8:], msgSessionD2C)
	copy(pkt[0x1C:], inner)
	return pkt
}

// newTestClient returns a Client wired up enough to exercise
// parseDatagram / drainContiguous / emit without any actual UDP. The
// internal counters and assembly state are reset to a fresh
// just-finished-bootstrap shape with `firstSubWire` as the first
// expected AV-seq value.
func newTestClient(firstSubWire uint16, quality string) *Client {
	c := &Client{
		nonce:    make([]byte, 8),
		quality:  quality,
		frames:   make(chan *Packet, 256),
		avBuffer: make(map[uint64]*pendingFrag),
	}
	if firstSubWire == 0 {
		c.wrap = wrapSeq{ext: 0x4000}
	} else {
		c.wrap = wrapSeq{ext: uint64(firstSubWire)}
	}
	c.avNextExt = c.wrap.ext
	c.avHighExt = c.wrap.ext - 1
	c.avSequenceAnchored = true
	c.avEpochFloor = c.wrap.ext - 1
	c.initACKTracking(c.wrap.ext - 1)
	return c
}

func TestFirstRuntimeMediaPreservesRepresentableInitialLoss(t *testing.T) {
	c := newTestClient(0x4000, "sd")

	payload := append([]byte{0, 0, 0, 1, 0x41, 0x88}, make([]byte, 16)...)
	payload = makeTrailerPayload(payload, false, 0x01, 1000)
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChInter, sub17: 0x01, subWire: 0x4002,
		totalFrags: 1, fragIdx: 0, paylen: uint16(len(payload)), frameNum: 1,
		payload: payload,
	}))

	if got := c.contiguousAckExt(); got != 0x3fff {
		t.Fatalf("ACK watermark=0x%x, want bootstrap watermark 0x3fff", got)
	}
	a, _ := ackFor(t, c)
	if !reflect.DeepEqual(a.Missing, []uint16{1, 2}) {
		t.Fatalf("initial-loss ACK=%+v, want missing offsets [1 2]", a)
	}
}

func TestQueuePacketReportsOutputQueueDrop(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	c.frames = make(chan *Packet, 1)
	if !c.queuePacket(&Packet{Codec: CodecH264}) {
		t.Fatal("first packet was not queued")
	}
	if c.queuePacket(&Packet{Codec: CodecH264}) {
		t.Fatal("second packet unexpectedly fit in full queue")
	}
	if got := c.stats.emitDrops.Load(); got != 1 {
		t.Fatalf("output queue drops=%d, want 1", got)
	}
}

func TestOutputQueueDropSuppressesDependentPFramesUntilIDR(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	c.frames = make(chan *Packet, 1)

	idr := []byte{0, 0, 0, 1, 0x65, 0x88}
	inter := []byte{0, 0, 0, 1, 0x61, 0x99}
	c.emitAU(idr, 1, innerChKey, 1, 1000, true)
	c.emitAU(inter, 2, innerChInter, 1, 1040, true) // full queue: reference AU lost

	first := drainAll(c)
	if len(first) != 1 || !first[0].IsKeyframe {
		t.Fatalf("initial output=%+v, want one IDR", first)
	}

	c.emitAU(inter, 3, innerChInter, 1, 1080, true)
	if got := drainAll(c); len(got) != 0 {
		t.Fatalf("forwarded %d dependent P-frames after a queue drop", len(got))
	}

	c.emitAU(idr, 4, innerChKey, 1, 1120, true)
	recoveryHead := drainAll(c)
	if len(recoveryHead) != 1 || !recoveryHead[0].IsKeyframe {
		t.Fatalf("recovery head=%+v, want one IDR", recoveryHead)
	}
	c.emitAU(inter, 5, innerChInter, 1, 1160, true)
	got := drainAll(c)
	if len(got) != 1 || got[0].IsKeyframe {
		t.Fatalf("post-IDR output=%+v, want one P-frame", got)
	}
	if got := c.stats.emitDrops.Load(); got != 1 {
		t.Fatalf("output queue drops=%d, want 1", got)
	}
	if got := c.stats.gopRecoveryDrop.Load(); got != 1 {
		t.Fatalf("GOP recovery drops=%d, want 1", got)
	}
}

func TestGappedReferencePFrameSuppressesGOPUntilIDR(t *testing.T) {
	c := newTestClient(0x4000, "hd") // non-strict is the production default
	idr := []byte{0, 0, 0, 1, 0x65, 0x88}
	inter := []byte{0, 0, 0, 1, 0x61, 0x99}

	// Establish a clean GOP, then discard its queued head so the assertions
	// below cover only the loss/recovery sequence.
	fixtureEmit(c, &pendingFrag{channel: innerChKey, frameNum: 1, totalFrags: 1,
		payload: makeTrailerPayload(idr, true, 1, 1000)})
	_ = drainAll(c)

	// A fragment-index gap makes frame 2 incomplete. It must not be emitted,
	// and frame 3 is dependent on that missing reference picture.
	fixtureEmit(c, &pendingFrag{channel: innerChInter, frameNum: 2, totalFrags: 3,
		fragIdx: 0, payload: inter})
	fixtureEmit(c, &pendingFrag{channel: innerChInter, frameNum: 2, totalFrags: 3,
		fragIdx: 2, payload: []byte{0xaa, 0xbb}})
	fixtureEmit(c, &pendingFrag{channel: innerChInter, frameNum: 2, totalFrags: 3,
		fragIdx: 3, payload: makeTrailerPayload([]byte{0xcc}, false, 1, 1040)})
	fixtureEmit(c, &pendingFrag{channel: innerChInter, frameNum: 3, totalFrags: 1,
		payload: makeTrailerPayload(inter, false, 1, 1080)})
	if got := drainAll(c); len(got) != 0 {
		t.Fatalf("forwarded %d P-frames after a gapped reference picture", len(got))
	}

	// A new IDR starts a self-contained prediction chain; following P-frames
	// may then be forwarded normally.
	fixtureEmit(c, &pendingFrag{channel: innerChKey, frameNum: 4, totalFrags: 1,
		payload: makeTrailerPayload(idr, true, 1, 1120)})
	fixtureEmit(c, &pendingFrag{channel: innerChInter, frameNum: 5, totalFrags: 1,
		payload: makeTrailerPayload(inter, false, 1, 1160)})
	got := drainAll(c)
	if len(got) != 2 || !got[0].IsKeyframe || got[1].IsKeyframe {
		t.Fatalf("recovery output=%+v, want IDR followed by P-frame", got)
	}
	if got := c.stats.gopRecoveryDrop.Load(); got != 1 {
		t.Fatalf("GOP recovery drops=%d, want 1", got)
	}
}

func TestAcceptedInterMediaCannotLeaveEmptyPendingAssembly(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	// Data-family video with an empty payload is rejected by decoding before
	// it can create framePending with an empty assembly buffer.
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChInter, b1: 0x00, subWire: 0x4000,
		totalFrags: 2, fragIdx: 0, paylen: 0, frameNum: 1,
	}))
	if c.interAsm.framePending {
		t.Fatal("zero-payload video created a pending inter-frame assembly")
	}

	// A trailer-only multi-fragment P-frame is accepted as media but reaches
	// the zero-data hard floor synchronously and resets the assembly.
	trailer := makeTrailerPayload(nil, false, 1, 1040)
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChInter, b1: 0x01, sub17: 0x01, subWire: 0x4000,
		totalFrags: 2, fragIdx: 1, paylen: uint16(len(trailer)), frameNum: 2,
		payload: trailer,
	}))
	if c.interAsm.framePending || len(c.interAsm.buf) != 0 {
		t.Fatalf("hard-dropped P-frame left pending assembly: %+v", c.interAsm)
	}
	if !c.gopPoisoned {
		t.Fatal("hard-dropped reference P-frame did not poison its GOP")
	}
}

// drainAll pulls every queued Packet without blocking and returns them.
func drainAll(c *Client) []*Packet {
	var out []*Packet
	for {
		select {
		case p := <-c.frames:
			out = append(out, p)
		default:
			return out
		}
	}
}

// dataFragPayload generates a deterministic n-byte slice with a
// fingerprint so the assembled AU can be validated against a known
// concatenation.
func dataFragPayload(seed byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = seed + byte(i%19)
	}
	return out
}

func TestExtendedMediaHeaderAssemblyAndACK(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	mainData := []byte{0, 0, 0, 1, 0x65, 0x88, 0x99}
	mainEnd := makeTrailerPayload([]byte{0xaa, 0xbb}, true, 0x01, 1000)
	c.parseDatagram(makeExtendedPkt(extendedFragSpec{
		b1: 0x08, channel: innerChKey, subFlag: 0,
		subWire: 0x4000, totalFrags: 2, fragIdx: 0,
		frameNum: 10, mediaFrameNo: 11, payload: mainData,
	}))
	c.parseDatagram(makeExtendedPkt(extendedFragSpec{
		b1: 0x09, channel: innerChKey, subFlag: 1,
		subWire: 0x4001, totalFrags: 2, fragIdx: 16,
		frameNum: 10, mediaFrameNo: 11, payload: mainEnd,
	}))

	subData := []byte{0, 0, 0, 1, 0x41, 0x44}
	subEnd := makeTrailerPayload([]byte{0xcc}, false, 0x01, 1040)
	c.parseDatagram(makeExtendedPkt(extendedFragSpec{
		b1: 0x0c, channel: innerChInter, subFlag: 0,
		subWire: 0x4002, totalFrags: 2, fragIdx: 0,
		frameNum: 11, mediaFrameNo: 12, payload: subData,
	}))
	c.parseDatagram(makeExtendedPkt(extendedFragSpec{
		b1: 0x0d, channel: innerChInter, subFlag: 1,
		subWire: 0x4003, totalFrags: 2, fragIdx: 16,
		frameNum: 11, mediaFrameNo: 12, payload: subEnd,
	}))

	packets := drainAll(c)
	if len(packets) != 2 {
		t.Fatalf("emitted %d packets, want one key-family and one inter-family AU", len(packets))
	}
	if !packets[0].IsKeyframe || packets[1].IsKeyframe {
		t.Fatalf("unexpected keyframe flags: first=%t second=%t", packets[0].IsKeyframe, packets[1].IsKeyframe)
	}
	if got := c.stats.extendedMediaParsed.Load(); got != 4 {
		t.Fatalf("extendedMediaParsed=%d, want 4", got)
	}
	if got := c.stats.extendedMediaDataPackets.Load(); got != 2 {
		t.Fatalf("extendedMediaDataPackets=%d, want 2", got)
	}
	if got := c.stats.extendedMediaEndPackets.Load(); got != 2 {
		t.Fatalf("extendedMediaEndPackets=%d, want 2", got)
	}
	if got := c.stats.extendedMediaRarePackets.Load(); got != 2 {
		t.Fatalf("extendedMediaRarePackets=%d, want 2", got)
	}
	if got := c.stats.sequenceSeenAndAssembled.Load(); got != 4 {
		t.Fatalf("sequenceSeenAndAssembled=%d, want 4", got)
	}
	if got := c.contiguousAckExt(); got != 0x4003 {
		t.Fatalf("ACK watermark=0x%x, want 0x4003", got)
	}
	if got := c.stats.ackSeenPending.Load(); got != 0 {
		t.Fatalf("ACK pending=%d, want 0", got)
	}
	if got := c.stats.missingFragmentsTotal.Load(); got != 0 {
		t.Fatalf("missingFragmentsTotal=%d, want 0", got)
	}
}

func TestCapturedRetransmissionsRecoverACKGap(t *testing.T) {
	// The captured failure's contiguous receive position was 33743. Deliver a
	// captured retransmitted inter fragment and one newer canonical fragment
	// first, leaving 33744 as the outstanding hole.
	c := newTestClient(33744, "hd")
	interRetrans := capturedRetransInter33745(t)
	media, ok := decodeNormalMedia(interRetrans[0x1c:])
	if !ok || media.wireChannel != innerChRetransInter || media.channel != innerChInter || !media.retransmit {
		t.Fatalf("captured inter classification=%+v ok=%t", media, ok)
	}
	c.parseDatagram(interRetrans)
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChInter, b1: 0x00, subWire: 33746,
		totalFrags: 7, fragIdx: 1, paylen: 32, frameNum: 2538028,
		payload: dataFragPayload(0x52, 32),
	}))
	before, _ := ackFor(t, c)
	if before.AVBase != 33743 || before.AVHigh != 33746 || !reflect.DeepEqual(before.Missing, []uint16{1}) {
		t.Fatalf("ACK before recovery=%+v", before)
	}

	audioRetrans := capturedRetransAudio33744(t)
	media, ok = decodeNormalMedia(audioRetrans[0x1c:])
	if !ok || media.wireChannel != innerChRetransAudio || media.channel != innerChAudio || !media.retransmit || !media.isAudio {
		t.Fatalf("captured audio classification=%+v ok=%t", media, ok)
	}
	c.parseDatagram(audioRetrans)

	after, _ := ackFor(t, c)
	if after.AVBase != 33743 || after.AVHigh != 33746 || len(after.Missing) != 0 {
		t.Fatalf("ACK after recovery=%+v", after)
	}
	if got := c.contiguousAckExt(); got != 33746 {
		t.Fatalf("ACK watermark=%d, want 33746", got)
	}
	if got := c.stats.ackSeenPending.Load(); got != 0 {
		t.Fatalf("ACK pending=%d, want 0", got)
	}
	if got := c.stats.ackAdvanced.Load(); got != 3 {
		t.Fatalf("ACK advanced=%d, want 3", got)
	}
	if got := c.stats.mediaRetransmissions.Load(); got != 2 {
		t.Fatalf("accepted media retransmissions=%d, want 2", got)
	}
	packets := drainAll(c)
	if len(packets) != 1 || packets[0].Codec != CodecAACADTS {
		t.Fatalf("recovered output=%+v, want one AAC unit", packets)
	}

	// Repeating the now-old retransmission is valid wire media, but it cannot
	// regress either ACK or assembly state and must not emit duplicate audio.
	c.parseDatagram(audioRetrans)
	if got := c.contiguousAckExt(); got != 33746 {
		t.Fatalf("duplicate retransmission regressed ACK watermark to %d", got)
	}
	if got := c.stats.ackDuplicateOrOld.Load(); got != 1 {
		t.Fatalf("duplicate/old count=%d, want 1", got)
	}
	if got := c.stats.deferredDrop.Load(); got != 1 {
		t.Fatalf("late assembler drop count=%d, want 1", got)
	}
	if got := len(drainAll(c)); got != 0 {
		t.Fatalf("duplicate retransmission emitted %d packets", got)
	}
}

func TestRetransmittedKeyframeAssemblesOnCanonicalChannel(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	data := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x99}
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChRetransKey, b1: 0x00, subWire: 0x4000,
		totalFrags: 2, fragIdx: 0, paylen: uint16(len(data)), frameNum: 10, payload: data,
	}))
	end := makeTrailerPayload([]byte{0xaa, 0xbb}, true, 1, 1000)
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChRetransKey, b1: 0x01, sub17: 0x01, subWire: 0x4001,
		totalFrags: 2, fragIdx: 16, paylen: uint16(len(end)), frameNum: 10, payload: end,
	}))

	packets := drainAll(c)
	if len(packets) != 1 || packets[0].Codec != CodecH264 || !packets[0].IsKeyframe || packets[0].Channel != innerChKey {
		t.Fatalf("retransmitted keyframe output=%+v", packets)
	}
	if got := c.contiguousAckExt(); got != 0x4001 {
		t.Fatalf("ACK watermark=0x%x, want 0x4001", got)
	}
	if got := c.stats.mediaRetransmissions.Load(); got != 2 {
		t.Fatalf("accepted media retransmissions=%d, want 2", got)
	}
}

func TestExtendedRetransmissionChannelsUseSameMediaSemantics(t *testing.T) {
	c := newTestClient(0x4000, "sd")
	audio := []byte{0xff, 0xf1, 0x50, 0x80, 0x00, 0xff, 0xfc}
	c.parseDatagram(makeExtendedPkt(extendedFragSpec{
		b1: 0x09, channel: innerChRetransAudio, subFlag: 1,
		subWire: 0x4000, totalFrags: 1, fragIdx: 16,
		frameNum: 20, mediaFrameNo: 4, payload: append(append([]byte(nil), audio...), make([]byte, 16)...),
	}))
	video := makeTrailerPayload([]byte{0, 0, 0, 1, 0x61, 0x88}, false, 1, 1040)
	c.parseDatagram(makeExtendedPkt(extendedFragSpec{
		b1: 0x0d, channel: innerChRetransInter, subFlag: 1,
		subWire: 0x4001, totalFrags: 1, fragIdx: 16,
		frameNum: 21, mediaFrameNo: 5, payload: video,
	}))

	packets := drainAll(c)
	if len(packets) != 2 || packets[0].Codec != CodecAACADTS || packets[1].Codec != CodecH264 || packets[1].Channel != innerChInter {
		t.Fatalf("extended retransmission output=%+v", packets)
	}
	if got := c.contiguousAckExt(); got != 0x4001 {
		t.Fatalf("ACK watermark=0x%x, want 0x4001", got)
	}
	if got := c.stats.mediaRetransmissions.Load(); got != 2 {
		t.Fatalf("accepted media retransmissions=%d, want 2", got)
	}
}

func TestMalformedRetransmissionChannelsDoNotACK(t *testing.T) {
	tests := map[string][]byte{
		"audio without ADTS": makePkt(fragSpec{
			channel: innerChRetransAudio, b1: 0x01, sub17: 0x01, subWire: 0x4000,
			totalFrags: 1, fragIdx: 16, paylen: 3, frameNum: 1, payload: []byte{1, 2, 3},
		}),
		"empty key data": makePkt(fragSpec{
			channel: innerChRetransKey, b1: 0x00, subWire: 0x4000,
			totalFrags: 1, fragIdx: 0, frameNum: 1,
		}),
		"unsupported inter family": makePkt(fragSpec{
			channel: innerChRetransInter, b1: 0x02, subWire: 0x4000,
			totalFrags: 1, fragIdx: 0, paylen: 3, frameNum: 1, payload: []byte{1, 2, 3},
		}),
		"unknown channel": makePkt(fragSpec{
			channel: 0x09, b1: 0x00, subWire: 0x4000,
			totalFrags: 1, fragIdx: 0, paylen: 3, frameNum: 1, payload: []byte{1, 2, 3},
		}),
		"extended fragment out of range": makeExtendedPkt(extendedFragSpec{
			b1: 0x08, channel: innerChRetransKey, subFlag: 0,
			subWire: 0x4000, totalFrags: 1, fragIdx: 1,
			frameNum: 1, mediaFrameNo: 1, payload: []byte{1, 2, 3},
		}),
	}
	for name, pkt := range tests {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(0x4000, "hd")
			c.parseDatagram(pkt)
			if got := c.contiguousAckExt(); got != 0x3fff {
				t.Fatalf("malformed packet advanced ACK watermark to 0x%x", got)
			}
			if got := c.stats.mediaRetransmissions.Load(); got != 0 {
				t.Fatalf("malformed packet counted as %d accepted retransmissions", got)
			}
			if len(c.avBuffer) != 0 || len(drainAll(c)) != 0 {
				t.Fatal("malformed packet reached assembly or output")
			}
		})
	}
}

func TestExtendedMediaRejectDoesNotACK(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	c.parseDatagram(makeExtendedPkt(extendedFragSpec{
		b1: 0x08, channel: innerChKey, subFlag: 0,
		subWire: 0x4000, totalFrags: 2, fragIdx: 2,
		frameNum: 10, mediaFrameNo: 99, payload: []byte{1, 2, 3},
	}))
	c.parseDatagram(makeExtendedPkt(extendedFragSpec{
		b1: 0x0d, channel: innerChInter, subFlag: 1,
		subWire: 0x4001, totalFrags: 1, fragIdx: 16,
		frameNum: 11, mediaFrameNo: 12, payload: []byte{1, 2, 3},
	}))

	if got := c.stats.extendedMediaCandidates.Load(); got != 2 {
		t.Fatalf("extendedMediaCandidates=%d, want 2", got)
	}
	if got := c.stats.extendedMediaRejected.Load(); got != 2 {
		t.Fatalf("extendedMediaRejected=%d, want 2", got)
	}
	if got := c.contiguousAckExt(); got != 0x3fff {
		t.Fatalf("rejected packet advanced ACK watermark to 0x%x", got)
	}
}

func TestExtendedMediaAudioAndVideoRemainContiguous(t *testing.T) {
	c := newTestClient(0x4000, "sd")
	audio := []byte{0xff, 0xf1, 0x50, 0x80, 0x00, 0xff, 0xfc}
	video := []byte{0, 0, 0, 1, 0x61, 0x88, 0x99}
	videoWire := makeTrailerPayload(video, false, 1, 1040)

	// Physical AAC captures interleave a shared frame number at offset 36 with
	// an independently advancing media-family counter at offset 40. Neither
	// channel is required to have the old frameNum+1 relationship.
	c.parseDatagram(makeExtendedPkt(extendedFragSpec{
		b1: 0x09, channel: innerChAudio, subFlag: 1,
		subWire: 0x4000, totalFrags: 1, fragIdx: 16,
		frameNum: 23, mediaFrameNo: 16, payload: append(append([]byte(nil), audio...), make([]byte, 16)...),
	}))
	c.parseDatagram(makeExtendedPkt(extendedFragSpec{
		b1: 0x0d, channel: innerChAudio, subFlag: 1,
		subWire: 0x4001, totalFrags: 1, fragIdx: 16,
		frameNum: 66, mediaFrameNo: 43, payload: append(append([]byte(nil), audio...), make([]byte, 16)...),
	}))
	c.parseDatagram(makeExtendedPkt(extendedFragSpec{
		b1: 0x09, channel: innerChInter, subFlag: 1,
		subWire: 0x4002, totalFrags: 1, fragIdx: 16,
		frameNum: 37, mediaFrameNo: 14, payload: videoWire,
	}))

	packets := drainAll(c)
	if len(packets) != 3 {
		t.Fatalf("emitted %d packets, want two AAC units followed by H.264", len(packets))
	}
	if packets[0].Codec != CodecAACADTS || packets[1].Codec != CodecAACADTS || packets[2].Codec != CodecH264 {
		t.Fatalf("unexpected codecs: first=%d second=%d third=%d", packets[0].Codec, packets[1].Codec, packets[2].Codec)
	}
	if !bytes.Equal(packets[0].Payload, audio) || !bytes.Equal(packets[1].Payload, audio) || !bytes.Equal(packets[2].Payload, video) {
		t.Fatal("extended complete-unit payload changed during assembly")
	}
	if !packets[2].HasCameraTime {
		t.Fatal("extended video FRAMEINFO timestamp was not preserved")
	}
	if got := c.contiguousAckExt(); got != 0x4002 {
		t.Fatalf("ACK watermark=0x%x, want 0x4002", got)
	}
	if got := c.stats.ackSeenPending.Load(); got != 0 {
		t.Fatalf("ACK pending=%d, want 0", got)
	}
}

func TestEndToEndMultiFragmentIDR(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	const totalFrags = 5 // 4 data + 1 end
	const frameNum = uint32(1)
	const tsMs = uint32(500)

	var wantPayload []byte
	for i := 0; i < 4; i++ {
		data := dataFragPayload(byte(0x10+i), 16)
		wantPayload = append(wantPayload, data...)
		c.parseDatagram(makePkt(fragSpec{
			channel:    innerChKey,
			b1:         0x00,
			subWire:    uint16(0x4000 + i),
			totalFrags: totalFrags,
			fragIdx:    uint16(i),
			paylen:     uint16(len(data)),
			frameNum:   frameNum,
			payload:    data,
		}))
	}

	// End fragment: b1=0x01 sub17=0x01, payload has trailer signature.
	// Use a 5-byte tail so the post-strip total slice is >= 5 (emitAU
	// gate).
	tail := []byte{0xCA, 0xFE, 0xBA, 0xBE, 0xDE}
	wantPayload = append(wantPayload, tail...)
	end := makeTrailerPayload(tail, true, 0x01, tsMs)
	c.parseDatagram(makePkt(fragSpec{
		channel:    innerChKey,
		b1:         0x01,
		sub17:      0x01,
		subWire:    uint16(0x4000 + 4),
		totalFrags: totalFrags,
		fragIdx:    4,
		paylen:     uint16(len(end)),
		frameNum:   frameNum,
		payload:    end,
	}))

	pkts := drainAll(c)
	if len(pkts) != 1 {
		t.Fatalf("emitted %d packets, want 1; pkts=%+v", len(pkts), pkts)
	}
	got := pkts[0]
	if got.Codec != CodecH264 {
		t.Fatalf("codec=0x%02x, want CodecH264=0x%02x", got.Codec, CodecH264)
	}
	if string(got.Payload) != string(wantPayload) {
		t.Fatalf("payload mismatch:\n got=%x\nwant=%x", got.Payload, wantPayload)
	}
	if !got.HasCameraTime || got.CameraTimeMS != tsMs {
		t.Fatalf("camera time=(%t,%d), want (true,%d)", got.HasCameraTime, got.CameraTimeMS, tsMs)
	}
}

// TestEndToEndMultiFragmentIDRPreservesCameraTime checks that transport
// assembly does not reinterpret camera timing.
func TestEndToEndMultiFragmentIDRPreservesCameraTime(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	sendIDR := func(frameNum uint32, baseSub uint16, tsMs uint32) {
		// 2 data + 1 end = totalFrags 3. tail of 8 bytes keeps the
		// post-strip AU >= 5 for the emitAU minimum-len gate.
		for i := 0; i < 2; i++ {
			c.parseDatagram(makePkt(fragSpec{
				channel:    innerChKey,
				b1:         0x00,
				subWire:    baseSub + uint16(i),
				totalFrags: 3,
				fragIdx:    uint16(i),
				paylen:     8,
				frameNum:   frameNum,
				payload:    dataFragPayload(byte(frameNum), 8),
			}))
		}
		end := makeTrailerPayload([]byte{0x99, 0x88, 0x77, 0x66, 0x55}, true, 0x01, tsMs)
		c.parseDatagram(makePkt(fragSpec{
			channel:    innerChKey,
			b1:         0x01,
			sub17:      0x01,
			subWire:    baseSub + 2,
			totalFrags: 3,
			fragIdx:    2,
			paylen:     uint16(len(end)),
			frameNum:   frameNum,
			payload:    end,
		}))
	}

	sendIDR(1, 0x4000, 100)
	sendIDR(2, 0x4003, 200)

	pkts := drainAll(c)
	if len(pkts) != 2 {
		t.Fatalf("emitted %d packets, want 2", len(pkts))
	}
	if !pkts[0].HasCameraTime || !pkts[1].HasCameraTime ||
		pkts[0].CameraTimeMS != 100 || pkts[1].CameraTimeMS != 200 {
		t.Fatalf("camera times=(%t,%d),(%t,%d)",
			pkts[0].HasCameraTime, pkts[0].CameraTimeMS,
			pkts[1].HasCameraTime, pkts[1].CameraTimeMS)
	}
}

// ---------- FRAMEINFO onlineNum handling -------------------------------

// HD quality is a readiness preference. A valid received frame is not
// dropped because its onlineNum differs from a local quality request.
func TestOnlineNumDoesNotFilterKeyFrames(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	hdData := dataFragPayload(0xAA, 16)
	c.parseDatagram(makePkt(fragSpec{
		channel:    innerChKey,
		b1:         0x00,
		subWire:    0x4000,
		totalFrags: 2,
		fragIdx:    0,
		paylen:     uint16(len(hdData)),
		frameNum:   10,
		payload:    hdData,
	}))
	hdTail := []byte{0x11, 0x22, 0x33, 0x44, 0x55}
	hdEnd := makeTrailerPayload(hdTail, true, 0x01, 1000)
	c.parseDatagram(makePkt(fragSpec{
		channel:    innerChKey,
		b1:         0x01,
		sub17:      0x01,
		subWire:    0x4001,
		totalFrags: 2,
		fragIdx:    1,
		paylen:     uint16(len(hdEnd)),
		frameNum:   10,
		payload:    hdEnd,
	}))

	// Another IDR follows with a different online-client count.
	sdData := dataFragPayload(0xBB, 16)
	c.parseDatagram(makePkt(fragSpec{
		channel:    innerChKey,
		b1:         0x00,
		subWire:    0x4002,
		totalFrags: 2,
		fragIdx:    0,
		paylen:     uint16(len(sdData)),
		frameNum:   11,
		payload:    sdData,
	}))
	sdTail := []byte{0x33, 0x44, 0x55, 0x66, 0x77}
	sdEnd := makeTrailerPayload(sdTail, true, 0x02, 1100)
	c.parseDatagram(makePkt(fragSpec{
		channel:    innerChKey,
		b1:         0x01,
		sub17:      0x01,
		subWire:    0x4003,
		totalFrags: 2,
		fragIdx:    1,
		paylen:     uint16(len(sdEnd)),
		frameNum:   11,
		payload:    sdEnd,
	}))

	pkts := drainAll(c)
	if len(pkts) != 2 {
		t.Fatalf("emitted %d packets, want 2 regardless of onlineNum; pkts=%+v", len(pkts), pkts)
	}
	want := append(append([]byte{}, hdData...), hdTail...)
	if string(pkts[0].Payload) != string(want) {
		t.Fatalf("HD payload mismatch:\n got=%x\nwant=%x", pkts[0].Payload, want)
	}
}

func TestHDRequestEmitsInterFramesRegardlessOfOnlineNum(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	// Single-fragment inter frame with onlineNum=2.
	sdEnd := makeTrailerPayload([]byte{0x55, 0x66, 0x77, 0x88, 0x99}, false, 0x02, 100)
	c.parseDatagram(makePkt(fragSpec{
		channel:    innerChInter,
		b1:         0x00,
		subWire:    0x4000,
		totalFrags: 1,
		fragIdx:    0,
		paylen:     uint16(len(sdEnd)),
		frameNum:   5,
		payload:    sdEnd,
	}))

	// Then another frame with onlineNum=1.
	hdEnd := makeTrailerPayload([]byte{0xCC, 0xDD, 0xEE, 0xFF, 0xAA}, false, 0x01, 200)
	c.parseDatagram(makePkt(fragSpec{
		channel:    innerChInter,
		b1:         0x00,
		subWire:    0x4001,
		totalFrags: 1,
		fragIdx:    0,
		paylen:     uint16(len(hdEnd)),
		frameNum:   6,
		payload:    hdEnd,
	}))

	pkts := drainAll(c)
	if len(pkts) != 2 {
		t.Fatalf("emitted %d packets, want 2", len(pkts))
	}
	if pkts[0].OnlineNum != 2 || pkts[1].OnlineNum != 1 {
		t.Fatalf("onlineNum values=%d/%d", pkts[0].OnlineNum, pkts[1].OnlineNum)
	}
}

func TestForceDrainReasonCountersAndWindow(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	c.avHighExt = 0x4009
	c.avBuffer[0x4001] = &pendingFrag{channel: 0xff, subExt: 0x4001}
	c.avBuffer[0x4002] = &pendingFrag{channel: 0xff, subExt: 0x4002}

	c.forceDrain()

	if got := c.stats.forceDrainFlush.Load(); got != 1 {
		t.Fatalf("forceDrainFlush=%d, want 1", got)
	}
	if got := c.stats.forceDrainEntries.Load(); got != 1 {
		t.Fatalf("forceDrainEntries=%d, want 1", got)
	}
	if _, ok := c.avBuffer[0x4002]; !ok {
		t.Fatal("entry inside the eight-packet reorder window was flushed")
	}
}

func TestForceDrainDoesNotAdvanceACKWatermark(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	send := func(channel byte, subWire uint16, frameNum uint32) {
		c.parseDatagram(makePkt(fragSpec{
			channel: channel, b1: 0x00,
			subWire: subWire, totalFrags: 1, fragIdx: 0,
			paylen: 8, frameNum: frameNum, payload: dataFragPayload(byte(frameNum), 8),
		}))
	}

	send(innerChKey, 0x4000, 1)
	send(innerChKey, 0x4002, 2) // leave 0x4001 missing
	if got := c.contiguousAckExt(); got != 0x4000 {
		t.Fatalf("ACK watermark after gap=0x%x, want 0x4000", got)
	}
	if got := c.stats.ackSeenPending.Load(); got != 1 {
		t.Fatalf("ackSeenPending=%d, want 1", got)
	}

	// The first call observes a new high-water value; five more calls
	// reach the stall threshold and flush the buffered 0x4002 entry.
	for range forceDrainStallTicks + 1 {
		c.forceDrain()
	}
	if got := c.avNextExt; got != 0x4003 {
		t.Fatalf("assembler cursor after forceDrain=0x%x, want 0x4003", got)
	}
	if got := c.contiguousAckExt(); got != 0x4000 {
		t.Fatalf("forceDrain advanced ACK watermark to 0x%x, want 0x4000", got)
	}

	// A retransmission can close the ACK gap even though the assembler
	// cursor has already moved past it and will classify it as deferred. Use
	// AF203's retransmitted-key channel rather than the original channel.
	send(innerChRetransKey, 0x4001, 3)
	if got := c.contiguousAckExt(); got != 0x4002 {
		t.Fatalf("ACK watermark after retransmit=0x%x, want 0x4002", got)
	}
	if got := c.stats.ackSeenPending.Load(); got != 0 {
		t.Fatalf("ackSeenPending after retransmit=%d, want 0", got)
	}
	if got := c.stats.ackAdvanced.Load(); got != 3 {
		t.Fatalf("ackAdvanced=%d, want 3", got)
	}
	if got := c.stats.deferredDrop.Load(); got != 1 {
		t.Fatalf("deferredDrop=%d, want 1", got)
	}

	send(innerChRetransKey, 0x4001, 3)
	if got := c.stats.ackDuplicateOrOld.Load(); got != 1 {
		t.Fatalf("ackDuplicateOrOld=%d, want 1", got)
	}
	if got := c.stats.mediaRetransmissions.Load(); got != 2 {
		t.Fatalf("accepted media retransmissions=%d, want 2", got)
	}
}

func TestACKTrackingCompressesOnePermanentGap(t *testing.T) {
	c := newTestClient(0x4000, "hd")
	c.initACKTracking(0x4000)

	for subExt := uint64(0x4002); subExt < 0x8000; subExt++ {
		c.markACKReceived(subExt)
	}
	if got := len(c.ackSeenRanges); got != 1 {
		t.Fatalf("ackSeenRanges=%d, want 1", got)
	}
	if got, want := c.stats.ackSeenPending.Load(), uint64(0x8000-0x4002); got != want {
		t.Fatalf("ackSeenPending=%d, want %d", got, want)
	}
	if got := c.stats.ackTrackingOverflow.Load(); got != 0 {
		t.Fatalf("ackTrackingOverflow=%d, want 0", got)
	}

	c.markACKReceived(0x4001)
	if got := c.contiguousAckExt(); got != 0x7fff {
		t.Fatalf("watermark=0x%x, want 0x7fff", got)
	}
	if got := len(c.ackSeenRanges); got != 0 {
		t.Fatalf("ackSeenRanges after gap closure=%d, want 0", got)
	}
	if got := c.stats.ackSeenPending.Load(); got != 0 {
		t.Fatalf("ackSeenPending after gap closure=%d, want 0", got)
	}
}

func TestACKTrackingCapsDisjointRanges(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	for i := uint64(0); i < maxACKSeenRanges+32; i++ {
		c.markACKReceived(0x4002 + i*2)
	}
	if got := len(c.ackSeenRanges); got != maxACKSeenRanges {
		t.Fatalf("ackSeenRanges=%d, want %d", got, maxACKSeenRanges)
	}
	if got := c.stats.ackSeenPending.Load(); got != maxACKSeenRanges {
		t.Fatalf("ackSeenPending=%d, want %d", got, maxACKSeenRanges)
	}
	if got := c.stats.ackTrackingOverflow.Load(); got != 32 {
		t.Fatalf("ackTrackingOverflow=%d, want 32", got)
	}
}

// TestChannelAsmInterleavedKeyAndInter — ch=0x05 IDR and ch=0x07 P-frame
// fragments arriving interleaved (in wire order) must NOT corrupt each
// other: the per-channel `channelAsm` state means each channel can
// reassemble independently even while the other is mid-frame.
//
// This is the regression for the "shared buffer would drop every
// partial IDR" comment near channelAsm's docstring.
//
// Wire sequence: IDR data on ch=0x05, then an inter frame on ch=0x07.
// then IDR-end on ch=0x05. The cross-channel arrival of the ch=0x07
// The inter frame triggers flushPendingIDR(), which emits the in-progress IDR
// buffer; we assert both AUs reach the consumer queue (the regression
// is "shared buffer would drop every partial IDR" — here neither side
// gets dropped).
func TestChannelAsmInterleavedKeyAndInter(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	// Embed an H.264 NAL-5 (IDR-slice) start code into the key-family data
	// so annexbContainsNALType(au, h264.NALUTypeIFrame) flags this AU
	// as a keyframe in emitAU. The bytes are: Annex-B start-code 00
	// 00 00 01, then
	// NAL header byte 0x65 (forbidden_zero_bit=0, nal_ref_idc=3,
	// nal_unit_type=5), then a few payload bytes.
	keyData := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE}
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChKey, b1: 0x00,
		subWire: 0x4000, totalFrags: 2, fragIdx: 0,
		paylen: uint16(len(keyData)), frameNum: 100, payload: keyData,
	}))
	// P-frame on ch=0x07 arrives between the IDR's data and end —
	// triggers flushPendingIDR() in emit().
	interEnd := makeTrailerPayload([]byte{0x77, 0x88, 0x99, 0xAA, 0xBB}, false, 0x01, 1500)
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChInter, b1: 0x00,
		subWire: 0x4001, totalFrags: 1, fragIdx: 0,
		paylen: uint16(len(interEnd)), frameNum: 200, payload: interEnd,
	}))
	// IDR's end fragment. keyAsm was reset by flushPendingIDR; this
	// end-fragment now arrives "orphaned" and hits the hard-floor
	// (expectedData=1, curAUDataCount=0) → dropped. The earlier
	// flushPendingIDR is what carries the IDR through.
	keyEnd := makeTrailerPayload([]byte{0xEE, 0xDD, 0xCC, 0xBB, 0xAA}, true, 0x01, 1000)
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChKey, b1: 0x01, sub17: 0x01,
		subWire: 0x4002, totalFrags: 2, fragIdx: 1,
		paylen: uint16(len(keyEnd)), frameNum: 100, payload: keyEnd,
	}))

	pkts := drainAll(c)
	if len(pkts) < 2 {
		t.Fatalf("emitted %d packets, want >= 2 (both keyframe and P-frame must survive)", len(pkts))
	}
	var sawKey, sawP bool
	for _, p := range pkts {
		if p.IsKeyframe {
			sawKey = true
		} else {
			sawP = true
		}
	}
	if !sawKey {
		t.Errorf("no keyframe in output; ch=0x05 IDR was lost to cross-channel interleave")
	}
	if !sawP {
		t.Errorf("no P-frame in output; ch=0x07 P-frame was lost to mid-IDR arrival")
	}
}

// TestFragIdxGapDetection — a missing middle data fragment must be
// counted as a frag-skip + frags-lost gap. We model "fragment idx=1
// lost on the wire" by sending only data fragments at fragIdx 0 and
// 2 (totalFrags=3, so 2 data + 1 end) over contiguous wire
// subWires. The fragIdx jump 0→2 against the asm's expectFragIdx=1
// is the gap signal the emit() no-trailer branch is testing for.
func TestFragIdxGapDetection(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	// Data fragment 0 of 3 (subWire 0x4000, fragIdx 0).
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChKey, b1: 0x00,
		subWire: 0x4000, totalFrags: 3, fragIdx: 0,
		paylen: 8, frameNum: 1, payload: dataFragPayload(0x10, 8),
	}))
	// Data fragment 2 of 3 (subWire 0x4001, fragIdx 2 — gap from 0→2).
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChKey, b1: 0x00,
		subWire: 0x4001, totalFrags: 3, fragIdx: 2,
		paylen: 8, frameNum: 1, payload: dataFragPayload(0x30, 8),
	}))
	// End fragment, fragIdx 16 (the camera reuses 16 as the end-marker;
	// here we just need a trailer-bearing fragment past the data).
	end := makeTrailerPayload([]byte{0x99, 0x88, 0x77, 0x66, 0x55}, true, 0x01, 50)
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChKey, b1: 0x01, sub17: 0x01,
		subWire: 0x4002, totalFrags: 3, fragIdx: 16,
		paylen: uint16(len(end)), frameNum: 1, payload: end,
	}))

	if c.stats.fragSkips.Load() == 0 {
		t.Errorf("fragSkips=0; gap from frag 0→2 should have been counted")
	}
	if c.stats.fragIdxGap.Load() == 0 {
		t.Errorf("fragIdxGap=0; gap from frag 0→2 should have been classified")
	}
	if c.stats.fragsLost.Load() == 0 {
		t.Errorf("fragsLost=0; at least one lost fragment expected")
	}
	if c.stats.vidDropped.Load() == 0 {
		t.Errorf("vidDropped=0; non-strict mode still increments on gapped IDR")
	}
}

// TestPaylenTruncatesPadding — when paylen is non-zero and smaller
// than (len(inner)-36), the assembler must trim trailing padding.
// This prevents synthesised start-code bytes in the padding from
// reaching the decoder.
func TestPaylenTruncatesPadding(t *testing.T) {
	c := newTestClient(0x4000, "hd")

	// inner has 16 bytes after offset 36, but paylen says only 4 are real.
	raw := []byte{0xAA, 0xBB, 0xCC, 0xDD /* padding: */, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1}
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChKey, b1: 0x00,
		subWire: 0x4000, totalFrags: 2, fragIdx: 0,
		paylen: 4, frameNum: 1, payload: raw,
	}))
	end := makeTrailerPayload([]byte{0x99, 0x88, 0x77, 0x66, 0x55}, true, 0x01, 1)
	c.parseDatagram(makePkt(fragSpec{
		channel: innerChKey, b1: 0x01, sub17: 0x01,
		subWire: 0x4001, totalFrags: 2, fragIdx: 1,
		paylen: uint16(len(end)), frameNum: 1, payload: end,
	}))

	pkts := drainAll(c)
	if len(pkts) != 1 {
		t.Fatalf("emitted %d, want 1", len(pkts))
	}
	want := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0x99, 0x88, 0x77, 0x66, 0x55}
	if string(pkts[0].Payload) != string(want) {
		t.Fatalf("payload=%x, want %x (padding past paylen=4 trimmed)", pkts[0].Payload, want)
	}
}

// Sanitized fixtures retain native header structure, not the checksum of
// private media that was removed. This independently mirrors 3be4f8.
func fixtureMediaChecksum(inner []byte, off int) {
	if len(inner) < off+20 {
		return
	}
	var sum uint16
	for i, b := range inner[off : off+20+int(binary.LittleEndian.Uint16(inner[off+8:]))] {
		if i == 10 || i == 11 {
			continue
		}
		if i == 0 {
			if ch, _, ok := normalizeMediaChannel(b); ok {
				b = ch
			}
		}
		sum += uint16(b)
	}
	binary.LittleEndian.PutUint16(inner[off+10:], sum)
}
func fixtureEmit(c *Client, p *pendingFrag) {
	if _, _, _, ok := stripFragmentMetadataTrailer(p.payload); ok {
		p.isEnd = true
		p.metadataLen = 16
	}
	c.emit(p)
}

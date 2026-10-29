package petlibro

// AVAPI BBR envelope and internal control contract, AF203 3.1.48:
// 3b5374 (dispatch), 3b4184 (receipt), 3b4c48 (controls), 3be4f8 (checksum).
// Codec recognition deliberately does not participate in transport receipt.
import (
	"bytes"
	"encoding/binary"
	"errors"
	"time"
)

const (
	controlSenderClear      uint32 = 0x22
	controlSenderCleared    uint32 = 0x23
	controlReceiveReset     uint32 = 0x24
	controlReceiveResetDone uint32 = 0x25
	internalFamily          uint16 = 0x1000
	applicationFamily       uint16 = 0x7000
	maxReliableBytes               = 128 * 1024 // native reliable receive capacity
	maxMediaFragments              = 257        // application's 256 KiB frame + metadata
)

func (c *Client) sessionInner(pkt []byte) ([]byte, bool) {
	if len(pkt) < 29 || !bytes.Equal(pkt[:3], []byte{4, 2, magicVersion}) || pkt[3] != flagsRecv ||
		binary.LittleEndian.Uint16(pkt[8:]) != msgSessionD2C {
		return nil, false
	}
	n := int(binary.LittleEndian.Uint16(pkt[4:])) - 12
	if n < 1 || n != len(pkt)-28 {
		return nil, false
	}
	if len(c.nonce) != 8 || !bytes.Equal(pkt[20:28], c.nonce) {
		return nil, false
	}
	return pkt[28:], true
}

// decodeMediaEnvelope accepts source data only, never FEC parity. Retransmit
// markers are normalized before the native additive checksum is evaluated.
func decodeMediaEnvelope(inner []byte) (m decodedMedia, reason string, ok bool) {
	if len(inner) < 36 || inner[0] != 12 || inner[1]&^byte(13) != 0 {
		return m, "header", false
	}
	format := binary.LittleEndian.Uint16(inner[2:])
	if format != 11 && format != 12 {
		return m, "format", false
	}
	off := 16
	m.b1 = inner[1]
	m.extended = m.b1&8 != 0
	if m.extended {
		off += 8
	}
	if len(inner) < off+20 {
		return m, "short_header", false
	}
	h := inner[off : off+20]
	m.wireChannel = h[0]
	var valid bool
	m.channel, m.retransmit, valid = normalizeMediaChannel(h[0])
	if !valid {
		return m, "channel", false
	}
	m.subFlag = h[1]
	m.subWire = binary.LittleEndian.Uint16(h[2:])
	m.totalFrags = binary.LittleEndian.Uint16(h[4:])
	m.fragIdx = binary.LittleEndian.Uint16(h[6:])
	m.payloadLen = binary.LittleEndian.Uint16(h[8:])
	m.frameNum = binary.LittleEndian.Uint32(h[12:])
	m.mediaFrameNo = binary.LittleEndian.Uint32(h[16:])
	m.isEnd = m.subFlag == 1
	m.isAudio = m.channel == innerChAudio
	if m.subFlag > 1 || (m.b1&1 != 0) != m.isEnd || m.totalFrags == 0 || m.totalFrags > maxMediaFragments ||
		m.payloadLen == 0 || m.payloadLen > 1024 || len(inner) < off+20+int(m.payloadLen) {
		return m, "structure", false
	}
	if (!m.isEnd && m.fragIdx >= m.totalFrags) || (m.isEnd && m.fragIdx > m.payloadLen) {
		return m, "fragment", false
	}
	m.payload = inner[off+20 : off+20+int(m.payloadLen)]
	m.extraLen = len(inner) - off - 20 - int(m.payloadLen)
	var sum uint16
	for i, b := range h {
		if i == 10 || i == 11 {
			continue
		}
		if i == 0 {
			b = m.channel
		}
		sum += uint16(b)
	}
	for _, b := range m.payload {
		sum += uint16(b)
	}
	if sum != binary.LittleEndian.Uint16(h[10:]) {
		return m, "checksum", false
	}
	return m, "accepted", true
}

type receiveReset struct {
	id, boundary uint16
	pending      bool
}
type peerOperation struct {
	id, boundary uint16
	frame        uint32
	set          bool
	response     []byte
}
type reliableSend struct {
	inner []byte
	last  time.Time
}
type reliableFragment struct {
	seq, total, index uint16
	frame             uint32
	family            uint16
	body              []byte
}

func controlBody(op uint32, id, boundary uint16, frame uint32) []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint32(b, op)
	binary.LittleEndian.PutUint16(b[4:], id)
	binary.LittleEndian.PutUint16(b[6:], boundary)
	binary.LittleEndian.PutUint32(b[8:], frame)
	return b
}

// All protocol mutations and outgoing reliable counters are owned by innerMu
// once runtime starts. Bootstrap owns the same state before goroutines start.
func (c *Client) sendReliable(family uint16, body []byte) error {
	if len(c.reliablePending) >= 256 {
		return errors.New("petlibro: reliable send capacity")
	}
	seq := c.reliableTX
	c.reliableTX++
	inner := innerData(c.icounter, family, seq, body)
	c.icounter++
	binary.LittleEndian.PutUint32(inner[28:], c.reliableGroup)
	c.reliableGroup++
	if c.reliablePending == nil {
		c.reliablePending = make(map[uint16]*reliableSend)
	}
	c.reliablePending[seq] = &reliableSend{inner: inner, last: time.Now()}
	if c.conn == nil {
		return errors.New("petlibro: transport closed")
	}
	return c.sendInner(inner)
}

func (c *Client) retryReliable(now time.Time) error {
	// Retransmit the original reliable sequence/message identity, not a new
	// semantic request. Native reset IDs make repeated requests idempotent.
	for _, p := range c.reliablePending {
		if now.Sub(p.last) >= defaultACKInterval {
			if err := c.sendInner(p.inner); err != nil {
				return err
			}
			p.last = now
		}
	}
	return nil
}

func (c *Client) acceptReliableACK(ack uint16) {
	if uint16((c.reliableTX-1)-ack) >= 0x8000 {
		return
	}
	for seq := range c.reliablePending {
		if uint16(ack-seq) < 0x8000 {
			delete(c.reliablePending, seq)
		}
	}
}

func decodeReliable(inner []byte) (f reliableFragment, ok bool) {
	if len(inner) < 36 || inner[0] != 12 || inner[1] != 0 {
		return f, false
	}
	if format := binary.LittleEndian.Uint16(inner[2:]); format != 11 && format != 12 {
		return f, false
	}
	f.family = binary.LittleEndian.Uint16(inner[16:])
	if f.family != internalFamily && f.family != applicationFamily {
		return f, false
	}
	f.seq = binary.LittleEndian.Uint16(inner[18:])
	f.total = binary.LittleEndian.Uint16(inner[20:])
	f.index = binary.LittleEndian.Uint16(inner[22:])
	n := int(binary.LittleEndian.Uint16(inner[24:]))
	f.frame = binary.LittleEndian.Uint32(inner[28:])
	if f.total == 0 || f.total > 128 || f.index >= f.total || n == 0 || n > 1024 || len(inner) != 36+n {
		return f, false
	}
	f.body = inner[36:]
	return f, true
}

func (c *Client) receiveReliable(f reliableFragment) {
	if !c.reliableRecvSet {
		return
	} // LOGIN establishes initial reliable receive position
	delta := uint16(f.seq - c.reliableRecvACK)
	if delta == 0 || delta >= 0x8000 {
		return
	}
	if c.reliableRX == nil {
		c.reliableRX = make(map[uint16]reliableFragment)
	}
	if _, exists := c.reliableRX[f.seq]; exists {
		return
	}
	if len(c.reliableRX) >= 256 || c.reliableBytes+len(f.body) > maxReliableBytes {
		return
	}
	f.body = append([]byte(nil), f.body...)
	c.reliableRX[f.seq] = f
	c.reliableBytes += len(f.body)
	for {
		next := c.reliableRecvACK + 1
		p, exists := c.reliableRX[next]
		if !exists {
			break
		}
		// +169e is completely delivered reliable data, not a copied fragment.
		if p.index != 0 || !c.completeReliable(next, p) {
			return
		}
	}
}

func (c *Client) completeReliable(first uint16, f reliableFragment) bool {
	var body []byte
	for i := uint16(0); i < f.total; i++ {
		p, ok := c.reliableRX[first+i]
		if !ok || p.index != i || p.total != f.total || p.frame != f.frame || p.family != f.family {
			return false
		}
		body = append(body, p.body...)
	}
	if len(body) > maxReliableBytes {
		return false
	}
	for i := uint16(0); i < f.total; i++ {
		p := c.reliableRX[first+i]
		c.reliableBytes -= len(p.body)
		delete(c.reliableRX, first+i)
	}
	for i := uint16(0); i < f.total; i++ {
		c.acceptReliableControl(first + i)
	}
	c.ackForce = true
	c.handleReliableMessage(f.family, body)
	return true
}

func (c *Client) resetMedia(boundary uint16, frame uint32, frameSet bool) {
	// Give an initial 0xffff boundary a non-negative extended representation.
	floor := uint64(boundary)
	if c.avSequenceAnchored {
		if ext, ok := c.wrap.extend(boundary); ok && ext > floor {
			floor = ext
		}
	}
	c.avSequenceAnchored = true
	c.avEpochFloor = floor
	c.avFrameFloor = frame
	c.avFrameFloorSet = frameSet
	c.wrap = wrapSeq{ext: floor + 1}
	c.avNextExt = floor + 1
	c.avHighExt = floor
	c.avBuffer = make(map[uint64]*pendingFrag)
	c.keyAsm.reset()
	c.interAsm.reset()
	c.audioAsm = audioAssembly{}
	c.gopPoisoned = true
	c.stallTicks = 0
	c.lastHighExt = floor
	c.avNextObserved.Store(floor + 1)
	c.initACKTracking(floor)
	c.ackForce = true
}

func (c *Client) handleReliableMessage(family uint16, body []byte) {
	if family != internalFamily || len(body) != 12 {
		return
	}
	op := binary.LittleEndian.Uint32(body)
	id := binary.LittleEndian.Uint16(body[4:])
	boundary := binary.LittleEndian.Uint16(body[6:])
	frame := binary.LittleEndian.Uint32(body[8:])
	switch op {
	case controlReceiveResetDone:
		if !c.reset.pending || id != c.reset.id || boundary != c.reset.boundary {
			return
		}
		c.resetMedia(boundary, frame, true)
		c.reset.pending = false
		if c.peerClear.set {
			// Reliable clear and reset completion can cross on the wire. Keep
			// the sender-authorized discard even if its control arrived first.
			if delta := uint16(c.peerClear.boundary - boundary); delta < 0x8000 {
				c.discardThrough(c.avEpochFloor+uint64(delta), c.peerClear.frame)
			}
		}
		pending := c.resetMediaPending
		c.resetMediaPending = nil
		for _, m := range pending {
			c.acceptMedia(m)
		}
	case controlSenderClear:
		if c.peerClear.set {
			if id == c.peerClear.id {
				if boundary == c.peerClear.boundary && frame == c.peerClear.frame {
					_ = c.sendReliable(internalFamily, c.peerClear.response)
				}
				return
			}
			if uint16(id-c.peerClear.id) >= 0x8000 {
				return
			}
		}
		if c.avSequenceAnchored {
			ext, ok := c.wrap.extend(boundary)
			if !ok || ext < c.avEpochFloor {
				return
			}
			c.discardThrough(ext, frame)
		} else if !c.reset.pending || uint16(boundary-c.reset.boundary) >= 0x8000 {
			return
		}
		resp := controlBody(controlSenderCleared, id, boundary, frame)
		c.peerClear = peerOperation{id: id, boundary: boundary, frame: frame, set: true, response: resp}
		_ = c.sendReliable(internalFamily, resp)
	case controlReceiveReset:
		// Peer resets its receive direction. We send no AV: acknowledge its
		// requested outbound floor without altering our inbound media epoch.
		if c.peerReset.set {
			if id == c.peerReset.id {
				if boundary == c.peerReset.boundary {
					_ = c.sendReliable(internalFamily, c.peerReset.response)
				}
				return
			}
			if uint16(id-c.peerReset.id) >= 0x8000 {
				return
			}
		}
		resp := controlBody(controlReceiveResetDone, id, boundary, 0xffffffff)
		c.peerReset = peerOperation{id: id, boundary: boundary, set: true, response: resp}
		_ = c.sendReliable(internalFamily, resp)
	}
}

func (c *Client) discardThrough(floor uint64, frame uint32) {
	// Explicit sender authorization, not inferred packet receipt or forceDrain.
	c.avEpochFloor = floor
	if !c.avFrameFloorSet || int32(frame-c.avFrameFloor) > 0 {
		c.avFrameFloor = frame
	}
	c.avFrameFloorSet = true
	for seq, p := range c.avBuffer {
		if seq <= floor || int32(p.frameNum-c.avFrameFloor) <= 0 {
			delete(c.avBuffer, seq)
		}
	}
	c.keyAsm.reset()
	c.interAsm.reset()
	c.audioAsm = audioAssembly{}
	c.gopPoisoned = true
	if c.avNextExt <= floor {
		c.avNextExt = floor + 1
		c.avNextObserved.Store(c.avNextExt)
	}
	if c.avHighExt < floor {
		c.avHighExt = floor
		c.wrap.advanceTo(uint16(floor))
	}
	c.ackMu.Lock()
	if floor > c.ackWatermarkExt {
		c.ackWatermarkExt = floor
		for len(c.ackSeenRanges) > 0 && c.ackSeenRanges[0].last <= floor {
			c.ackSeenRanges = c.ackSeenRanges[1:]
		}
		if len(c.ackSeenRanges) > 0 && c.ackSeenRanges[0].first <= floor {
			c.ackSeenRanges[0].first = floor + 1
		}
		for len(c.ackSeenRanges) > 0 && c.ackSeenRanges[0].first == c.ackWatermarkExt+1 {
			c.ackWatermarkExt = c.ackSeenRanges[0].last
			c.ackSeenRanges = c.ackSeenRanges[1:]
		}
		c.ackSeenPending = 0
		for _, r := range c.ackSeenRanges {
			c.ackSeenPending += r.last - r.first + 1
		}
	}
	if c.ackReportedExt < floor {
		c.ackReportedExt = floor
	}
	if c.ackHighExt < floor {
		c.ackHighExt = floor
	}
	c.ackEpoch++
	c.ackForce = true
	c.stats.ackWatermark.Store(c.ackWatermarkExt)
	c.stats.ackHigh.Store(c.ackHighExt)
	c.stats.ackSeenPending.Store(c.ackSeenPending)
	c.stats.ackSeenRanges.Store(uint64(len(c.ackSeenRanges)))
	c.ackMu.Unlock()
}

package petlibro

// Audio shares AV ordering with video, but its bytes are interpreted only once
// a complete frame has been assembled. The final fragment carries metadataLen,
// not an ADTS header and not necessarily the integer 16 as a fragment index.
type audioAssembly struct {
	frame       uint32
	total, next uint16
	data        []byte
}

func (c *Client) emitAudio(e *pendingFrag) {
	a := &c.audioAsm
	if a.data == nil || a.frame != e.frameNum {
		*a = audioAssembly{frame: e.frameNum, total: e.totalFrags}
		if e.fragIdx != 0 {
			return
		}
		a.data = make([]byte, 0, 1024)
	}
	if e.fragIdx != a.next || e.totalFrags != a.total || len(a.data)+len(e.payload) > maxMediaFragments*1024 {
		*a = audioAssembly{}
		return
	}
	n := len(e.payload)
	if e.isEnd {
		n -= int(e.metadataLen)
	}
	a.data = append(a.data, e.payload[:n]...)
	a.next++
	if !e.isEnd {
		return
	}
	data := a.data
	*a = audioAssembly{}
	// Validate every ADTS frame, including its declared length. A transport
	// receipt remains valid even if this application-level check fails.
	for rest := data; len(rest) > 0; {
		if len(rest) < 7 || rest[0] != 0xff || rest[1]&0xf6 != 0xf0 {
			return
		}
		n := int(rest[3]&3)<<11 | int(rest[4])<<3 | int(rest[5]>>5)
		header := 7
		if rest[1]&1 == 0 {
			header = 9
		}
		if n < header || n > len(rest) {
			return
		}
		if (rest[2]>>2)&15 >= 13 || (rest[2]&1)<<2|rest[3]>>6 == 0 {
			return
		}
		rest = rest[n:]
	}
	if len(data) == 0 {
		return
	}
	c.queuePacket(&Packet{Codec: CodecAACADTS, Payload: data, FrameNo: c.emitAudioSeq})
	c.emitAudioSeq++
}

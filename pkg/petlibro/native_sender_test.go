package petlibro

import (
	"fmt"
	"testing"
)

type nativeSenderNode struct {
	sent          uint32
	retransmitted bool
	bytes         int
}
type nativeSenderModel struct {
	flight, resend   map[uint16]nativeSenderNode
	timed            []uint16
	rtt, rto, margin uint16
	scans            int
}

func newNativeSenderModel() *nativeSenderModel {
	return &nativeSenderModel{flight: map[uint16]nativeSenderNode{}, resend: map[uint16]nativeSenderNode{}, rtt: 20, rto: 200, margin: 100}
}
func nativeQueueBefore(a, b uint16) bool {
	// Queue helper 0x3b0fbc uses this high-bit/low-15 comparison,
	// not the subtly different comparator 0x3b3d04 at half-space.
	if (a^b)&0x8000 == 0 {
		return a < b
	}
	return a&0x7fff > b&0x7fff
}

// FUN_003b5374 case 9: group-prefix purge, high-node/reference guard,
// sorted offset walk, RTT/4 retry eligibility, then positive retirement.
// Reliable queues and subsequent BBR feedback are independent and not modeled.
func (s *nativeSenderModel) ack(a transportACK, now uint32) bool {
	threshold := a.AVBase & 0xfff0
	for _, q := range []map[uint16]nativeSenderNode{s.flight, s.resend} {
		for seq := range q {
			if nativeQueueBefore(seq&0xfff0, threshold) {
				delete(q, seq)
			}
		}
	}
	timed := s.timed[:0]
	for _, seq := range s.timed {
		if !nativeQueueBefore(seq, a.AVBase+1) {
			timed = append(timed, seq)
		}
	}
	s.timed = timed
	h, found := s.flight[a.AVHigh]
	if !found {
		h, found = s.resend[a.AVHigh]
	}
	ref := now
	if found {
		if h.retransmitted && uint16(now-h.sent) <= s.rtt {
			return false
		}
		ref = h.sent
	} else if len(a.Missing) == 0 {
		return false
	}
	high := uint32(a.AVHigh)
	if a.AVHigh < a.AVBase {
		high += 65536
	}
	j := 0
	for ext := uint32(a.AVBase) + 1; ext <= high; ext++ {
		s.scans++
		seq := uint16(ext)
		if j < len(a.Missing) && uint16(a.AVBase+a.Missing[j]) == seq {
			j++
			if n, ok := s.flight[seq]; ok && n.sent < ref && ref-n.sent > uint32(s.rtt>>2) {
				delete(s.flight, seq)
				s.resend[seq] = n
			}
		} else {
			delete(s.flight, seq)
			delete(s.resend, seq)
		}
	}
	return true
}

// FUN_003b2b68 timed queue: too-young front is reinserted at FRONT and
// terminates the pass; modulo-16 age, not an arbitrary payload expiry.
func (s *nativeSenderModel) tick(now uint32) int {
	timeout := s.rtt
	if s.rto > timeout {
		timeout = s.rto
	}
	count := 0
	for len(s.timed) > 0 {
		seq := s.timed[0]
		n, ok := s.flight[seq]
		// Real watchdog nodes carry their own sent timestamp. Test nodes use
		// matching flight timestamps and no duplicate watchdogs.
		if ok && uint32(uint16(now-n.sent)) < uint32(timeout)+uint32(s.margin) {
			break
		}
		s.timed = s.timed[1:]
		if ok {
			delete(s.flight, seq)
			s.resend[seq] = n
			count++
		}
	}
	if count > 0 {
		v := uint32(s.rto) * 2
		if v > 30000 {
			v = 30000
		}
		s.rto = uint16(v)
	}
	return count
}

func TestNativeSenderNativeACKBranches(t *testing.T) {
	t.Run("same_batch_nack_uses_timed_retry", func(t *testing.T) {
		s := newNativeSenderModel()
		s.flight[0x4007] = nativeSenderNode{sent: 100, bytes: 1024}
		s.flight[0x4016] = nativeSenderNode{sent: 100, bytes: 1024}
		s.timed = []uint16{0x4007, 0x4016}
		if !s.ack(transportACK{AVBase: 0x4006, AVHigh: 0x4016, Missing: []uint16{1}}, 150) || len(s.resend) != 0 || len(s.flight) != 1 {
			t.Fatal(s)
		}
		if s.tick(399) != 0 || s.tick(400) != 1 || len(s.resend) != 1 {
			t.Fatal("timed retry guard")
		}
	})
	t.Run("high_absent_with_nacks_uses_now", func(t *testing.T) {
		s := newNativeSenderModel()
		s.flight[7] = nativeSenderNode{sent: 100}
		if !s.ack(transportACK{AVBase: 6, AVHigh: 20, Missing: []uint16{1}}, 150) || len(s.resend) != 1 {
			t.Fatal(s)
		}
	})
	t.Run("high_absent_no_nacks_skips_interval", func(t *testing.T) {
		s := newNativeSenderModel()
		s.flight[7] = nativeSenderNode{sent: 100}
		if s.ack(transportACK{AVBase: 6, AVHigh: 20}, 150) || len(s.flight) != 1 {
			t.Fatal(s)
		}
	})
	t.Run("fresh_retransmitted_high_skips_interval", func(t *testing.T) {
		s := newNativeSenderModel()
		s.flight[7] = nativeSenderNode{sent: 100}
		s.flight[20] = nativeSenderNode{sent: 140, retransmitted: true}
		if s.ack(transportACK{AVBase: 6, AVHigh: 20}, 150) || len(s.flight) != 2 {
			t.Fatal(s)
		}
	})
	t.Run("prefix_purge_precedes_guard", func(t *testing.T) {
		s := newNativeSenderModel()
		s.flight[7] = nativeSenderNode{sent: 100}
		s.flight[0x17] = nativeSenderNode{sent: 100}
		if s.ack(transportACK{AVBase: 0x16, AVHigh: 0x25}, 150) || len(s.flight) != 1 {
			t.Fatal(s)
		}
	})
	t.Run("wrap_missing_offset", func(t *testing.T) {
		s := newNativeSenderModel()
		s.flight[0xffff] = nativeSenderNode{sent: 100}
		s.flight[1] = nativeSenderNode{sent: 140}
		s.ack(transportACK{AVBase: 0xfffe, AVHigh: 1, Missing: []uint16{1}}, 200)
		if len(s.resend) != 1 || len(s.flight) != 0 {
			t.Fatal(s)
		}
	})
	t.Run("young_watchdog_front_blocks_older_tail", func(t *testing.T) {
		s := newNativeSenderModel()
		s.flight[7] = nativeSenderNode{sent: 900}
		s.flight[8] = nativeSenderNode{sent: 100}
		s.timed = []uint16{7, 8}
		if s.tick(1000) != 0 || len(s.timed) != 2 {
			t.Fatal(s)
		}
	})
}

func TestNativeSenderPinnedGapDoesNotImplyGrowingSenderQueue(t *testing.T) {
	s := newNativeSenderModel()
	s.flight[0x4007] = nativeSenderNode{sent: 100, bytes: 1024}
	for seq := uint16(0x4008); seq < 0x4800; seq++ {
		now := uint32(seq) * 10
		s.flight[seq] = nativeSenderNode{sent: now}
		s.ack(transportACK{AVBase: 0x4006, AVHigh: seq, Missing: []uint16{1}}, now+10)
		// Simulate successful resend transmission but a receiver refusing it.
		if n, ok := s.resend[0x4007]; ok {
			delete(s.resend, 0x4007)
			n.sent = now
			n.retransmitted = true
			s.flight[0x4007] = n
		}
		if len(s.flight)+len(s.resend) != 1 {
			t.Fatalf("sender retained %d nodes", len(s.flight)+len(s.resend))
		}
	}
	t.Log(fmt.Sprintf("one retained payload; interval scans=%d", s.scans))
}

func TestNativeSenderSkippedBootstrapRetirementLeavesPartialPrefix(t *testing.T) {
	s := newNativeSenderModel()
	for seq := uint16(0x4000); seq <= 0x4006; seq++ {
		s.flight[seq] = nativeSenderNode{sent: 140, retransmitted: true, bytes: 1024}
	}
	// Bootstrap can advertise all these positions, but high's fresh retry
	// prevents native interval retirement. Go nevertheless commits max.
	if s.ack(transportACK{AVBase: 0x3fff, AVHigh: 0x4006}, 150) {
		t.Fatal("guard bypass")
	}
	s.flight[0x4016] = nativeSenderNode{sent: 200, bytes: 1024}
	s.ack(transportACK{AVBase: 0x4006, AVHigh: 0x4016, Missing: []uint16{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}}, 250)
	if len(s.flight) != 7 {
		t.Fatalf("prefix nodes=%d", len(s.flight))
	}
	t.Log("7 already-advertised nodes remain below pinned base; bounded partial-group retention, NOT 60% occupancy")
}

func TestNativeSenderBBRRepeatedGapAccounting(t *testing.T) {
	// Narrow input-accounting model of 3bb614 -> first block of 3bbec0.
	// It does not simulate the full BBR state machine or assert a rate outcome.
	pending := map[uint16]uint32{}
	var delivered, nackVolume uint64
	apply := func(a transportACK) {
		var newly uint32
		missing := map[uint16]bool{}
		for _, off := range a.Missing {
			missing[a.AVBase+off] = true
		}
		for seq, n := range pending {
			if uint16(seq-a.AVBase) <= uint16(a.AVHigh-a.AVBase) && !missing[seq] {
				newly += n
				delete(pending, seq)
			}
		}
		delivered += uint64(newly)
		if newly != 0 {
			nackVolume += uint64(len(a.Missing)) * 1024
		}
	}
	a := transportACK{AVBase: 0x4006, Missing: []uint16{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}}
	for seq := uint16(0x4007); seq <= 0x4015; seq++ {
		pending[seq] = 1024
	}
	for seq := uint16(0x4016); seq < 0x4016+40; seq++ {
		pending[seq] = 1024
		a.AVHigh = seq
		apply(a)
	}
	if delivered != 40*1024 || nackVolume != 40*15*1024 || len(pending) != 15 {
		t.Fatal(delivered, nackVolume, len(pending))
	}
	before := nackVolume
	for n := 0; n < 40; n++ {
		apply(a)
	}
	if nackVolume != before {
		t.Fatal("zero-newly-accounted reports should skip BBR volume update")
	}
	t.Logf("40 newly-accounting ACKs: delivered=%d, repeated-gap volume=%d; duplicate ACKs add no volume", delivered, nackVolume)
}

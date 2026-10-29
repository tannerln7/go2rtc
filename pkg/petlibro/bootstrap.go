package petlibro

import (
	"encoding/binary"
	"fmt"
	"time"
)

// Internal reliable 24/25 establishes the AV receive epoch before START.
// Application IOCtrls remain in their observed order, but are NOT reset IDs.
type bootstrapCmd struct {
	channelFamily uint16
	payload       []byte
	startsMedia   bool
}

func (c *Client) debugIOCtrlResponse(inner []byte) {
	if !c.verbose {
		return
	}
	f, ok := decodeReliable(inner)
	if !ok || f.index != 0 || len(f.body) < 4 {
		return
	}
	log.Trace().Str("physical_session_id", c.sessionID).Uint16("family", f.family).
		Uint32("opcode", binary.LittleEndian.Uint32(f.body)).Uint16("message_sequence", f.seq).
		Int("body_length", len(f.body)).Msg("petlibro reliable response received")
}

func (c *Client) bootstrapIOCtrls() []bootstrapCmd {
	cmds := []bootstrapCmd{
		{channelFamily: applicationFamily, payload: ioctlBody12(ioctlGetVideoModeReq)},
		{channelFamily: applicationFamily, payload: ioctlBody12(ioctlGetStreamCtrlReq)},
		{channelFamily: applicationFamily, payload: ioctlBody12(ioctlGetAudioOutFormatReq)},
		{channelFamily: applicationFamily, payload: ioctlBody(ioctlInnerSendDataDelay, []byte{0, 0})},
		{channelFamily: applicationFamily, payload: ioctlBody12(ioctlStart), startsMedia: true},
	}
	if c.audio {
		body := ioctlBody12(ioctlAudioOn)
		body[4] = 1
		cmds = append(cmds, bootstrapCmd{channelFamily: applicationFamily, payload: body})
	}
	return cmds
}

func (c *Client) sendBootstrapCommand(cm bootstrapCmd) error {
	if err := c.sendReliable(cm.channelFamily, cm.payload); err != nil {
		return err
	}
	if cm.startsMedia {
		c.mediaStartSent.Store(true)
	}
	return nil
}

func (c *Client) pumpBootstrap(deadline time.Time, untilReset bool) error {
	for time.Now().Before(deadline) {
		now := time.Now()
		if err := c.retryReliable(now); err != nil {
			return err
		}
		if c.reportDue(now) {
			if err := c.sendReceiveReport(now); err != nil {
				return err
			}
		}
		if untilReset && !c.reset.pending {
			return nil
		}
		wait := min(20*time.Millisecond, time.Until(deadline))
		_ = c.conn.SetReadDeadline(time.Now().Add(wait))
		buf := make([]byte, 2048)
		n, addr, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			if e, ok := err.(interface{ Timeout() bool }); ok && e.Timeout() {
				continue
			}
			return err
		}
		if addr.Port != c.cam.Port || !addr.IP.Equal(c.cam.IP) {
			continue
		}
		c.handleEncryptedDatagram(buf[:n])
	}
	if untilReset {
		return fmt.Errorf("petlibro: receive-reset completion timed out")
	}
	return nil
}

func (c *Client) bootstrap() error {
	tick := uint32(0xc000) // captured initial probe clock, independent of AV epoch
	if err := c.sendInner(innerTimingFeedback(c.icounter, 0x1f, tick, 4)); err != nil {
		return err
	}
	c.icounter++
	c.acceptReliableControl(initialReliableRecvACK) // next D2C reliable sequence is 0
	c.initACKTracking(0xffff)
	if err := c.sendReceiveReport(time.Now()); err != nil {
		return err
	}
	if err := c.sendInner(innerTimingProbe(c.icounter, tick)); err != nil {
		return err
	}
	c.icounter++

	// This was formerly mislabeled as qualityHD/qualitySD. Both qualities use
	// the same reset contract. Resolution is OEM application configuration and
	// congestion-selected; the camera adapter interprets SPS readiness.
	c.reset = receiveReset{id: 1, boundary: 0x3fff, pending: true}
	if err := c.sendReliable(internalFamily, controlBody(controlReceiveReset, c.reset.id, c.reset.boundary, 0)); err != nil {
		return err
	}
	if err := c.pumpBootstrap(time.Now().Add(3*time.Second), true); err != nil {
		return err
	}
	for _, cm := range c.bootstrapIOCtrls() {
		if err := c.sendBootstrapCommand(cm); err != nil {
			return err
		}
		if err := c.pumpBootstrap(time.Now().Add(40*time.Millisecond), false); err != nil {
			return err
		}
	}
	// The SAME classifier and receipt set have processed every bootstrap media
	// datagram. Never advertise an unreceived position through a collected max.
	if err := c.sendReceiveReport(time.Now()); err != nil {
		return err
	}
	_ = c.conn.SetReadDeadline(time.Time{})
	return nil
}

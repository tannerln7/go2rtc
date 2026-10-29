package petlibro

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestBootstrapIOCtrlOrder(t *testing.T) {
	wantIDs := []uint32{
		ioctlGetVideoModeReq, ioctlGetStreamCtrlReq,
		ioctlGetAudioOutFormatReq, ioctlInnerSendDataDelay, ioctlStart,
	}
	cmds := (&Client{}).bootstrapIOCtrls()
	if len(cmds) != len(wantIDs) {
		t.Fatalf("got %d commands, want %d", len(cmds), len(wantIDs))
	}
	for i, wantID := range wantIDs {
		if got := binary.LittleEndian.Uint32(cmds[i].payload); got != wantID {
			t.Errorf("command %d ID=0x%04x, want 0x%04x", i, got, wantID)
		}
	}
}

func TestBootstrapAudioStartsAfterVideo(t *testing.T) {
	cmds := (&Client{audio: true}).bootstrapIOCtrls()
	if len(cmds) < 2 {
		t.Fatalf("audio bootstrap has %d commands", len(cmds))
	}
	start, audio := cmds[len(cmds)-2], cmds[len(cmds)-1]
	if got := binary.LittleEndian.Uint32(start.payload); got != ioctlStart || !start.startsMedia {
		t.Fatalf("penultimate command ID=0x%04x startsMedia=%t, want IPCAM_START", got, start.startsMedia)
	}
	if got := binary.LittleEndian.Uint32(audio.payload); got != ioctlAudioOn {
		t.Fatalf("final command ID=0x%04x, want AUDIOSTART 0x%04x", got, ioctlAudioOn)
	}
	if len(audio.payload) != 12 || audio.payload[4] != 1 {
		t.Fatalf("AUDIOSTART payload=% x, want enabled channel 1", audio.payload)
	}
}

func TestBootstrapAVAPIPayloadShapes(t *testing.T) {
	cmds := (&Client{}).bootstrapIOCtrls()

	delay := cmds[len(cmds)-2].payload
	if len(delay) != 6 || binary.LittleEndian.Uint32(delay) != ioctlInnerSendDataDelay ||
		binary.LittleEndian.Uint16(delay[4:]) != 0 {
		t.Fatalf("send-delay IOCtrl=% x, want ff 00 00 00 00 00", delay)
	}

	start := cmds[len(cmds)-1].payload
	if len(start) != 12 || binary.LittleEndian.Uint32(start) != ioctlStart {
		t.Fatalf("IPCAM_START IOCtrl=% x, want 4-byte type plus 8-byte payload", start)
	}
	if !bytes.Equal(start[4:], make([]byte, 8)) {
		t.Fatalf("IPCAM_START SMsgAVIoctrlAVStream=% x, want channel 0 and zero reserved bytes", start[4:])
	}
}

// The captured bodies formerly called HD and SD both reset AV sequencing.
func TestReceiveResetBodyFields(t *testing.T) {
	body := controlBody(controlReceiveReset, 1, 0x3fff, 0)
	want := []byte{0x24, 0, 0, 0, 1, 0, 0xff, 0x3f, 0, 0, 0, 0}
	if !bytes.Equal(body, want) {
		t.Fatalf("reset=%x want=%x", body, want)
	}
	for _, quality := range []string{"hd", "sd"} {
		cmds := (&Client{quality: quality}).bootstrapIOCtrls()
		if cmds[0].channelFamily != applicationFamily || binary.LittleEndian.Uint32(cmds[0].payload) != ioctlGetVideoModeReq {
			t.Fatal("quality must not choose a reset boundary")
		}
	}
}

package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func framed(payload []byte) []byte {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	return append(hdr[:], payload...)
}

func helloConsumerPayload(channel string, caps byte) []byte {
	p := append([]byte{msgHelloConsumer, byte(len(channel))}, channel...)
	if caps != 0 {
		p = append(p, caps)
	}
	return p
}

// readWireMsg reads one length-prefixed message off a consumer connection.
func readWireMsg(t *testing.T, c net.Conn) []byte {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	payload, _, err := readMsg(c)
	if err != nil {
		t.Fatalf("readMsg: %v", err)
	}
	return payload
}

// TestAudioFansOutOnlyToCapsSubscribers runs two consumers against the hub —
// one wire-v2 with the audio caps bit, one v1 without — publishes a frame and
// an audio chunk, and checks who got what and in which order.
func TestAudioFansOutOnlyToCapsSubscribers(t *testing.T) {
	h := newHub()

	dial := func(caps byte) net.Conn {
		client, server := net.Pipe()
		go h.handleConsumer(server, 16)
		if _, err := client.Write(framed(helloConsumerPayload("cam", caps))); err != nil {
			t.Fatal(err)
		}
		return client
	}
	withAudio := dial(capAudio)
	videoOnly := dial(0)
	defer withAudio.Close()
	defer videoOnly.Close()
	time.Sleep(50 * time.Millisecond) // let both hellos register

	audioMsgBytes := append([]byte{msgAudio}, bytes.Repeat([]byte{0xAA}, 32)...)
	frameMsgBytes := append([]byte{msgFrame}, bytes.Repeat([]byte{0xBB}, 32)...)
	h.publishAudio("cam", framed(audioMsgBytes))
	h.publish("cam", framed(frameMsgBytes))

	// The audio-capable door gets the chunk first, then the frame.
	if got := readWireMsg(t, withAudio); got[0] != msgAudio {
		t.Fatalf("audio consumer: first message type 0x%02x, want AUDIO", got[0])
	}
	if got := readWireMsg(t, withAudio); got[0] != msgFrame {
		t.Fatalf("audio consumer: second message type 0x%02x, want FRAME", got[0])
	}
	// The v1 door must only ever see the frame.
	if got := readWireMsg(t, videoOnly); got[0] != msgFrame {
		t.Fatalf("video-only consumer got type 0x%02x, want FRAME", got[0])
	}
}

func TestAudioQueueDropsOldest(t *testing.T) {
	s := newSubscriber(true, 3)
	for i := 0; i < 6; i++ {
		s.pushAudio([]byte{byte(i)})
	}
	if len(s.audio) != 3 {
		t.Fatalf("queue len %d, want 3", len(s.audio))
	}
	if s.audio[0][0] != 3 || s.audio[2][0] != 5 {
		t.Fatalf("queue kept %v, want the newest chunks 3..5", s.audio)
	}
	if s.audioDrop != 3 {
		t.Fatalf("audioDrop = %d, want 3", s.audioDrop)
	}
}

func TestVideoStaysDropToLatest(t *testing.T) {
	s := newSubscriber(false, 16)
	s.set([]byte{1})
	s.set([]byte{2})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest == nil || s.latest[0] != 2 {
		t.Fatalf("latest = %v, want the newest frame only", s.latest)
	}
}

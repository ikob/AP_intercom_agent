// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"net"
	"testing"

	"github.com/emiago/diago/media"
	"github.com/pion/rtp"
)

type packetCaptureWriter struct {
	packets []*rtp.Packet
}

func (w *packetCaptureWriter) WriteRTP(packet *rtp.Packet) error {
	copyPacket := *packet
	copyPacket.Payload = append([]byte(nil), packet.Payload...)
	w.packets = append(w.packets, &copyPacket)
	return nil
}

func TestPCMURTPWriterPacketizesContinuous20msFrames(t *testing.T) {
	capture := &packetCaptureWriter{}
	writer := newPCMURTPWriter(media.NewRTPPacketWriter(capture, media.CodecAudioUlaw))
	first := bytes.Repeat([]byte{0x11}, audioPCMUFrameBytes)
	second := bytes.Repeat([]byte{0x22}, audioPCMUFrameBytes)
	if err := writer.WritePCMU(first); err != nil {
		t.Fatal(err)
	}
	if err := writer.WritePCMU(second); err != nil {
		t.Fatal(err)
	}
	if len(capture.packets) != 2 {
		t.Fatalf("packet count = %d, want 2", len(capture.packets))
	}
	if got := capture.packets[0]; got.PayloadType != 0 || !got.Marker || !bytes.Equal(got.Payload, first) {
		t.Fatalf("first packet = %+v", got.Header)
	}
	if got := capture.packets[1]; got.PayloadType != 0 || got.Marker || !bytes.Equal(got.Payload, second) {
		t.Fatalf("second packet = %+v", got.Header)
	}
	if delta := capture.packets[1].Timestamp - capture.packets[0].Timestamp; delta != audioPCMUFrameBytes {
		t.Fatalf("timestamp delta = %d, want %d", delta, audioPCMUFrameBytes)
	}
}

func TestAudioEndpointFromSDP(t *testing.T) {
	endpoint, err := audioEndpointFromSDP([]byte(
		"v=0\r\n" +
			"c=IN IP4 192.0.2.10\r\n" +
			"m=audio 20364 RTP/AVP 0\r\n" +
			"c=IN IP4 192.0.2.11\r\n" +
			"a=rtpmap:0 PCMU/8000\r\n" +
			"m=video 8080 HTTP jpeg\r\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	want := &net.UDPAddr{IP: net.ParseIP("192.0.2.11"), Port: 20364}
	if !endpoint.IP.Equal(want.IP) || endpoint.Port != want.Port {
		t.Fatalf("endpoint = %s, want %s", endpoint, want)
	}
}

func TestAudioEndpointFromSDPRejectsNonPCMU(t *testing.T) {
	_, err := audioEndpointFromSDP([]byte(
		"v=0\r\nc=IN IP4 192.0.2.10\r\nm=audio 20364 RTP/AVP 8\r\n",
	))
	if err == nil {
		t.Fatal("expected non-PCMU SDP to be rejected")
	}
}

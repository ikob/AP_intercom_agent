// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestRelayIncomingAudioPublishesPCMUPayload(t *testing.T) {
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		receiver.Close()
		t.Fatal(err)
	}
	defer sender.Close()

	agent := &Agent{audioHub: NewAudioHub()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go agent.relayIncomingAudio(ctx, done, receiver, "call-test", "interphone0")
	defer func() {
		cancel()
		receiver.Close()
		<-done
	}()

	subscription := agent.audioHub.Subscribe(context.Background())
	defer subscription.Close()
	want := bytes.Repeat([]byte{0x42}, audioPCMUFrameBytes)
	packet := rtp.Packet{Header: rtp.Header{
		Version:        2,
		PayloadType:    0,
		SequenceNumber: 17,
		Timestamp:      320,
		SSRC:           1234,
	}, Payload: want}
	wire, err := packet.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.WriteToUDP(wire, receiver.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-subscription.Packets():
		if !bytes.Equal(got, want) {
			t.Fatalf("payload starts %x", got[:8])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for PCMU payload")
	}
}

// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/emiago/diago/media"
	"github.com/pion/rtp"
)

type pcmuRTPWriter struct {
	packetWriter *media.RTPPacketWriter
	marker       bool
}

func newPCMURTPWriter(packetWriter *media.RTPPacketWriter) *pcmuRTPWriter {
	if packetWriter == nil {
		return nil
	}
	return &pcmuRTPWriter{packetWriter: packetWriter, marker: true}
}

func (w *pcmuRTPWriter) WritePCMU(frame []byte) error {
	if w == nil || w.packetWriter == nil {
		return errors.New("PCMU RTP writer is unavailable")
	}
	if len(frame) != audioPCMUFrameBytes {
		return fmt.Errorf("invalid PCMU frame length %d", len(frame))
	}
	_, err := w.packetWriter.WriteSamples(frame, audioPCMUFrameBytes, w.marker, media.CodecAudioUlaw.PayloadType)
	w.marker = false
	return err
}

type udpRTPWriter struct {
	socket *net.UDPConn
	remote *net.UDPAddr
}

func (w *udpRTPWriter) WriteRTP(packet *rtp.Packet) error {
	if w == nil || w.socket == nil || w.remote == nil {
		return errors.New("UDP RTP destination is unavailable")
	}
	wire, err := packet.Marshal()
	if err != nil {
		return fmt.Errorf("marshal RTP packet: %w", err)
	}
	if _, err := w.socket.WriteToUDP(wire, w.remote); err != nil {
		return fmt.Errorf("send RTP packet to %s: %w", w.remote, err)
	}
	return nil
}

func newIncomingPCMURTPWriter(socket *net.UDPConn, remote *net.UDPAddr) talkbackFrameWriter {
	if socket == nil || remote == nil {
		return nil
	}
	packetWriter := media.NewRTPPacketWriter(&udpRTPWriter{socket: socket, remote: remote}, media.CodecAudioUlaw)
	return newPCMURTPWriter(packetWriter)
}

// audioEndpointFromSDP returns the first PCMU RTP destination in an SDP body.
// A media-level connection line takes precedence over the session connection.
func audioEndpointFromSDP(body []byte) (*net.UDPAddr, error) {
	var sessionHost string
	var audioHost string
	var audioPort int
	foundAudio := false
	seenMedia := false
	inAudio := false

	normalized := strings.ReplaceAll(string(body), "\r\n", "\n")
	for _, rawLine := range strings.Split(normalized, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "m=") {
			seenMedia = true
			inAudio = false
			fields := strings.Fields(line)
			if len(fields) < 4 || !strings.EqualFold(strings.TrimPrefix(fields[0], "m="), "audio") || foundAudio {
				continue
			}
			if !strings.EqualFold(fields[2], "RTP/AVP") {
				return nil, fmt.Errorf("audio uses unsupported protocol %q", fields[2])
			}
			pcmu := false
			for _, format := range fields[3:] {
				if format == "0" {
					pcmu = true
					break
				}
			}
			if !pcmu {
				return nil, fmt.Errorf("audio has no PCMU payload type: %q", line)
			}
			portText := strings.SplitN(fields[1], "/", 2)[0]
			port, err := strconv.Atoi(portText)
			if err != nil || port < 1 || port > 65535 {
				return nil, fmt.Errorf("invalid audio port %q", fields[1])
			}
			audioPort = port
			foundAudio = true
			inAudio = true
			continue
		}
		if strings.HasPrefix(line, "c=") {
			host, err := sdpConnectionHost(line)
			if err != nil {
				return nil, err
			}
			switch {
			case !seenMedia:
				sessionHost = host
			case inAudio:
				audioHost = host
			}
		}
	}
	if !foundAudio {
		return nil, errors.New("SDP has no PCMU audio media")
	}
	if audioHost == "" {
		audioHost = sessionHost
	}
	if audioHost == "" {
		return nil, errors.New("SDP has no connection address for audio")
	}
	return &net.UDPAddr{IP: net.ParseIP(audioHost), Port: audioPort}, nil
}

package main

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/rs/zerolog/log"
)

// ULaw to PCM16 conversion table
var ulawToPcm16 = buildULawTable()

func buildULawTable() [256]int16 {
	var table [256]int16
	for i := 0; i < 256; i++ {
		table[i] = ulawDecode(byte(i))
	}
	return table
}

// ulawDecode decodes a single ULaw byte to PCM16
func ulawDecode(ulaw byte) int16 {
	ulaw = ^ulaw
	sign := int16(ulaw & 0x80)
	exponent := int16((ulaw >> 4) & 0x07)
	mantissa := int16(ulaw & 0x0F)

	sample := int16((((mantissa << 3) + 0x84) << exponent) - 0x84)
	if sign != 0 {
		return -sample
	}
	return sample
}

// generateSilenceUlaw generates a ulaw silence frame
func generateSilenceUlaw(size int) []byte {
	silence := make([]byte, size)
	// 0xFF is ulaw encoded silence (0 in linear)
	for i := range silence {
		silence[i] = 0xFF
	}
	return silence
}

// AudioBridge handles receiving RTP audio from SIP calls and buffering for mixing
type AudioBridge struct {
	session                   *CallSession
	rtpConn                   *net.UDPConn
	cancel                    context.CancelFunc
	localPort                 int
	remoteAddr                *net.UDPAddr
	frameQueue                chan []byte
	backchannelSeqNum         uint16      // Sequence number for backchannel RTP packets
	backchannelTimestamp      uint32      // Timestamp for backchannel RTP packets
	backchannelMutex          sync.Mutex  // Protect backchannel counters
	lastBackchannelPacketTime time.Time   // Track last time we sent a backchannel packet
	backchannelActive         chan []byte // Channel for backchannel audio from RTSP clients
}

// NewAudioBridge creates a new audio bridge for transcoding
func NewAudioBridge(session *CallSession) (*AudioBridge, error) {
	// Listen on a random UDP port for RTP
	addr, err := net.ResolveUDPAddr("udp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("failed to resolve UDP address: %w", err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on UDP: %w", err)
	}

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	bridge := &AudioBridge{
		session:           session,
		rtpConn:           conn,
		localPort:         localAddr.Port,
		frameQueue:        make(chan []byte, 10), // Buffer up to 10 frames (200ms)
		backchannelActive: make(chan []byte, 10), // Buffer for backchannel audio from RTSP
	}

	return bridge, nil
}

// Start begins receiving RTP packets and forwarding to WebRTC
func (ab *AudioBridge) Start(ctx context.Context) error {
	ctx, ab.cancel = context.WithCancel(ctx)

	log.Info().
		Str("call_id", ab.session.callID).
		Int("rtp_port", ab.localPort).
		Msg("Starting audio bridge")

	// Buffer for RTP packets
	buffer := make([]byte, 1500)

	// Goroutine to receive RTP packets from SIP caller
	go func() {
		defer ab.rtpConn.Close()

		// Statistics
		var packetsReceived uint64
		var lastLog time.Time

		for {
			select {
			case <-ctx.Done():
				log.Info().
					Str("call_id", ab.session.callID).
					Uint64("packets", packetsReceived).
					Msg("Audio bridge stopped")
				return
			default:
			}

			// Set read deadline to allow checking context
			ab.rtpConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

			n, remoteAddr, err := ab.rtpConn.ReadFromUDP(buffer)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				log.Error().Err(err).Msg("Failed to read RTP packet")
				continue
			}

			// Store remote address for first packet (for NAT traversal)
			if ab.remoteAddr == nil {
				ab.remoteAddr = remoteAddr
				log.Info().
					Str("call_id", ab.session.callID).
					Str("remote_addr", remoteAddr.String()).
					Int("local_port", ab.localPort).
					Msg("Learned remote RTP address - backchannel can now send audio to SIP caller")
			}

			packetsReceived++

			// Parse RTP packet
			packet := &rtp.Packet{}
			if err := packet.Unmarshal(buffer[:n]); err != nil {
				log.Debug().Err(err).Msg("Failed to parse RTP packet")
				continue
			}

			// Process audio payload (ULaw -> Opus)
			if err := ab.processAudio(packet); err != nil {
				log.Debug().Err(err).Msg("Failed to process audio")
				continue
			}

			// Log statistics every 5 seconds
			if time.Since(lastLog) > 5*time.Second {
				log.Debug().
					Str("call_id", ab.session.callID).
					Uint64("packets", packetsReceived).
					Uint32("ssrc", packet.SSRC).
					Msg("Audio bridge stats")
				lastLog = time.Now()
			}
		}
	}()

	// Goroutine to send RTP packets back to SIP caller (backchannel or silence keepalive)
	go ab.backchannelSender(ctx)

	return nil
}

// processAudio queues audio frames for the mixer
func (ab *AudioBridge) processAudio(packet *rtp.Packet) error {
	// Make a copy of the payload
	frame := make([]byte, len(packet.Payload))
	copy(frame, packet.Payload)

	// Try to add to queue, drop if full
	select {
	case ab.frameQueue <- frame:
	default:
		// Queue full, drop oldest frame and add new one
		select {
		case <-ab.frameQueue:
			ab.frameQueue <- frame
		default:
		}
	}

	return nil
}

// GetLatestFrame returns the next audio frame from the queue
func (ab *AudioBridge) GetLatestFrame() []byte {
	select {
	case frame := <-ab.frameQueue:
		return frame
	default:
		return nil
	}
}

// Stop stops the audio bridge
func (ab *AudioBridge) Stop() {
	if ab.cancel != nil {
		ab.cancel()
	}
	if ab.rtpConn != nil {
		ab.rtpConn.Close()
	}
	if ab.frameQueue != nil {
		close(ab.frameQueue)
	}
}

// GetLocalPort returns the local RTP port
func (ab *AudioBridge) GetLocalPort() int {
	return ab.localPort
}

// SendToSIP queues audio data to be sent to the SIP caller (backchannel from RTSP to SIP)
func (ab *AudioBridge) SendToSIP(payload []byte) error {
	// Try to queue the backchannel audio, drop if channel is full
	select {
	case ab.backchannelActive <- payload:
		return nil
	default:
		// Channel full, drop the packet
		return fmt.Errorf("backchannel queue full, dropping packet")
	}
}

// backchannelSender sends RTP packets back to the SIP caller
// It prioritizes backchannel audio from RTSP clients, but sends silence if no audio is available
// This keepalive prevents SIP endpoints from timing out the call
func (ab *AudioBridge) backchannelSender(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Millisecond) // Send packets every 20ms
	defer ticker.Stop()

	silenceFrame := generateSilenceUlaw(160) // 160 samples = 20ms at 8kHz

	log.Info().
		Str("call_id", ab.session.callID).
		Msg("Backchannel sender started - will send silence to prevent call timeout")

	for {
		select {
		case <-ctx.Done():
			log.Info().
				Str("call_id", ab.session.callID).
				Msg("Backchannel sender stopped")
			return
		case <-ticker.C:
			// Wait for remote address to be learned
			if ab.remoteAddr == nil {
				continue
			}

			var payload []byte

			// Check if we have backchannel audio from RTSP client
			select {
			case payload = <-ab.backchannelActive:
				// Use backchannel audio
			default:
				// No backchannel audio, use silence
				payload = silenceFrame
			}

			// Send the RTP packet
			ab.backchannelMutex.Lock()

			packet := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    0, // PCMU
					SequenceNumber: ab.backchannelSeqNum,
					Timestamp:      ab.backchannelTimestamp,
					SSRC:           0x87654321, // Different SSRC for backchannel
					Marker:         false,
				},
				Payload: payload,
			}

			// Increment sequence number and timestamp for next packet
			ab.backchannelSeqNum++
			ab.backchannelTimestamp += uint32(len(payload)) // Increment by payload size (samples)

			// Marshal the packet
			data, err := packet.Marshal()
			if err != nil {
				ab.backchannelMutex.Unlock()
				log.Error().Err(err).Msg("Failed to marshal RTP packet")
				continue
			}

			// Send to SIP endpoint
			_, err = ab.rtpConn.WriteToUDP(data, ab.remoteAddr)
			ab.backchannelMutex.Unlock()

			if err != nil {
				log.Debug().Err(err).Msg("Failed to send RTP packet to SIP")
				continue
			}

			// Log successful send (periodically to avoid spam)
			if ab.backchannelSeqNum%500 == 0 {
				log.Debug().
					Str("call_id", ab.session.callID).
					Str("remote_addr", ab.remoteAddr.String()).
					Uint16("seq", ab.backchannelSeqNum).
					Uint32("timestamp", ab.backchannelTimestamp).
					Msg("SIP return audio keepalive active")
			}
		}
	}
}

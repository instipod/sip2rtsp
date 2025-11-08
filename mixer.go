package main

import (
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	mixerBufferSize = 960 // 20ms at 48kHz for internal processing
	ulawFrameSize   = 160 // 20ms at 8kHz ulaw
)

// AudioMixer mixes multiple SIP call audio streams into a single ulaw stream
type AudioMixer struct {
	sessions     map[string]*CallSession
	sessionsMux  sync.RWMutex
	outputBuffer chan []byte
	stopCh       chan struct{}
	running      bool
	runningMux   sync.Mutex
}

// NewAudioMixer creates a new audio mixer
func NewAudioMixer() *AudioMixer {
	return &AudioMixer{
		sessions:     make(map[string]*CallSession),
		outputBuffer: make(chan []byte, 100),
		stopCh:       make(chan struct{}),
	}
}

// AddSession adds a call session to the mixer
func (m *AudioMixer) AddSession(callID string, session *CallSession) {
	m.sessionsMux.Lock()
	defer m.sessionsMux.Unlock()
	m.sessions[callID] = session
	log.Info().Str("call_id", callID).Int("total_sessions", len(m.sessions)).Msg("Added session to mixer")
}

// RemoveSession removes a call session from the mixer
func (m *AudioMixer) RemoveSession(callID string) {
	m.sessionsMux.Lock()
	defer m.sessionsMux.Unlock()
	delete(m.sessions, callID)
	log.Info().Str("call_id", callID).Int("total_sessions", len(m.sessions)).Msg("Removed session from mixer")
}

// Start starts the mixer
func (m *AudioMixer) Start() {
	m.runningMux.Lock()
	if m.running {
		m.runningMux.Unlock()
		return
	}
	m.running = true
	m.runningMux.Unlock()

	go m.mixLoop()
	log.Info().Msg("Audio mixer started")
}

// Stop stops the mixer
func (m *AudioMixer) Stop() {
	m.runningMux.Lock()
	if !m.running {
		m.runningMux.Unlock()
		return
	}
	m.running = false
	m.runningMux.Unlock()

	close(m.stopCh)
	log.Info().Msg("Audio mixer stopped")
}

// GetOutputChannel returns the channel with mixed audio frames
func (m *AudioMixer) GetOutputChannel() <-chan []byte {
	return m.outputBuffer
}

// mixLoop continuously mixes audio from all active sessions
func (m *AudioMixer) mixLoop() {
	ticker := time.NewTicker(20 * time.Millisecond) // 20ms frames
	defer ticker.Stop()

	silenceFrame := generateSilenceUlaw(ulawFrameSize)

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			frame := m.mixFrame()
			if frame == nil {
				// No active sessions, send silence
				frame = silenceFrame
			}

			select {
			case m.outputBuffer <- frame:
			default:
				// Buffer full, drop frame
				log.Debug().Msg("Mixer output buffer full, dropping frame")
			}
		}
	}
}

// mixFrame mixes audio from all active sessions into a single frame
func (m *AudioMixer) mixFrame() []byte {
	m.sessionsMux.RLock()
	sessionCount := len(m.sessions)

	if sessionCount == 0 {
		m.sessionsMux.RUnlock()
		return nil
	}

	// Collect audio frames from all sessions
	var frames [][]byte
	for _, session := range m.sessions {
		if frame := session.GetLatestAudioFrame(); frame != nil {
			frames = append(frames, frame)
		}
	}
	m.sessionsMux.RUnlock()

	if len(frames) == 0 {
		return nil
	}

	// If only one session, return its frame directly
	if len(frames) == 1 {
		return frames[0]
	}

	// Mix multiple frames
	// Convert all ulaw frames to PCM16, mix, then convert back to ulaw
	mixedPCM := make([]int32, ulawFrameSize)

	for _, frame := range frames {
		// Ensure frame is the right size
		frameLen := len(frame)
		if frameLen > ulawFrameSize {
			frameLen = ulawFrameSize
		}

		for i := 0; i < frameLen; i++ {
			// Decode ulaw to PCM16 and accumulate
			pcmSample := ulawToPcm16[frame[i]]
			mixedPCM[i] += int32(pcmSample)
		}
	}

	// Convert mixed PCM back to ulaw with clipping
	mixedUlaw := make([]byte, ulawFrameSize)
	for i := 0; i < ulawFrameSize; i++ {
		// Average the mixed samples
		avgSample := int16(mixedPCM[i] / int32(len(frames)))

		// Clip to int16 range
		if avgSample > 32767 {
			avgSample = 32767
		} else if avgSample < -32768 {
			avgSample = -32768
		}

		// Encode back to ulaw
		mixedUlaw[i] = pcm16ToUlaw(avgSample)
	}

	return mixedUlaw
}

// pcm16ToUlaw encodes a PCM16 sample to ulaw
func pcm16ToUlaw(pcm int16) byte {
	// Get sign
	sign := byte(0x80)
	if pcm < 0 {
		pcm = -pcm
		sign = 0x00
	}

	// Convert to 14-bit range
	if pcm > 32635 {
		pcm = 32635
	}
	pcm += 0x84 // Add bias

	// Find exponent
	exponent := byte(7)
	for i := byte(0); i < 8; i++ {
		if pcm <= (0x1F << (i + 3)) {
			exponent = i
			break
		}
	}

	// Get mantissa
	mantissa := byte((pcm >> (exponent + 3)) & 0x0F)

	// Compose ulaw byte
	ulaw := ^(sign | (exponent << 4) | mantissa)

	return ulaw
}

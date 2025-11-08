package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/pion/rtp"
	"github.com/rs/zerolog/log"
)

// RTSPServer serves audio from SIP calls via RTSP
type RTSPServer struct {
	server  *gortsplib.Server
	streams map[string]*RTSPStream // indexed by extension
	mu      sync.RWMutex
	cancel  context.CancelFunc
}

// RTSPStream represents a single RTSP stream for an extension
type RTSPStream struct {
	extension            string
	session              *CallSession // nil when no active call
	stream               *gortsplib.ServerStream
	streamWithBackchan   *gortsplib.ServerStream // Stream with backchannel support
	cancel               context.CancelFunc
	sessionMutex         sync.RWMutex
	backchannelActive    bool // Track if a client is sending backchannel audio
	backchannelRequested bool // Track if backchannel was requested in DESCRIBE
}

// NewRTSPServer creates a new RTSP server
func NewRTSPServer(listenAddr string) (*RTSPServer, error) {
	rs := &RTSPServer{
		streams: make(map[string]*RTSPStream),
	}

	// Create RTSP server with UDP transport support
	rs.server = &gortsplib.Server{
		Handler:        rs,
		RTSPAddress:    listenAddr,
		UDPRTPAddress:  ":8000",
		UDPRTCPAddress: ":8001",
	}

	return rs, nil
}

// Start starts the RTSP server
func (rs *RTSPServer) Start(ctx context.Context) error {
	_, rs.cancel = context.WithCancel(ctx)

	// Start the server
	go func() {
		if err := rs.server.StartAndWait(); err != nil {
			log.Error().Err(err).Msg("RTSP server error")
		}
	}()

	// Wait a moment for server to initialize
	time.Sleep(100 * time.Millisecond)

	log.Info().
		Str("address", rs.server.RTSPAddress).
		Msg("RTSP server started")
	return nil
}

// AddStaticStream adds a new static stream for an extension (serves silence until a call is attached)
func (rs *RTSPServer) AddStaticStream(extension string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	// Check if stream already exists
	if _, exists := rs.streams[extension]; exists {
		log.Warn().Str("extension", extension).Msg("Stream already exists")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Create standard stream with PCMU format
	pcmuFormat := &format.G711{
		PayloadTyp:   0,
		MULaw:        true,
		SampleRate:   8000,
		ChannelCount: 1,
	}

	stream := gortsplib.NewServerStream(rs.server, &description.Session{
		Medias: []*description.Media{
			{
				Type:    description.MediaTypeAudio,
				Formats: []format.Format{pcmuFormat},
			},
		},
	})

	// Create stream with backchannel support (two tracks)
	pcmuFormatRecv := &format.G711{
		PayloadTyp:   0,
		MULaw:        true,
		SampleRate:   8000,
		ChannelCount: 1,
	}
	pcmuFormatSend := &format.G711{
		PayloadTyp:   0,
		MULaw:        true,
		SampleRate:   8000,
		ChannelCount: 1,
	}

	streamWithBackchan := gortsplib.NewServerStream(rs.server, &description.Session{
		Medias: []*description.Media{
			{
				Type:    description.MediaTypeAudio,
				Formats: []format.Format{pcmuFormatRecv},
			},
			{
				Type:          description.MediaTypeAudio,
				Formats:       []format.Format{pcmuFormatSend},
				IsBackChannel: true,
			},
		},
	})

	rtspStream := &RTSPStream{
		extension:          extension,
		session:            nil, // No session initially
		stream:             stream,
		streamWithBackchan: streamWithBackchan,
		cancel:             cancel,
	}

	rs.streams[extension] = rtspStream

	// Start streaming goroutine for this extension
	go rs.streamAudio(ctx, rtspStream)

	log.Info().
		Str("extension", extension).
		Str("rtsp_path", "/"+extension).
		Msg("RTSP stream added")
}

// AttachSession attaches a call session to an existing stream
func (rs *RTSPServer) AttachSession(extension string, session *CallSession) {
	rs.mu.RLock()
	rtspStream, exists := rs.streams[extension]
	rs.mu.RUnlock()

	if !exists {
		log.Error().Str("extension", extension).Msg("Stream not found for attaching session")
		return
	}

	rtspStream.sessionMutex.Lock()
	rtspStream.session = session
	rtspStream.sessionMutex.Unlock()

	log.Info().
		Str("extension", extension).
		Str("call_id", session.callID).
		Msg("Session attached to RTSP stream")
}

// DetachSession detaches a call session from a stream (stream continues serving silence)
func (rs *RTSPServer) DetachSession(extension string) {
	rs.mu.RLock()
	rtspStream, exists := rs.streams[extension]
	rs.mu.RUnlock()

	if !exists {
		log.Error().Str("extension", extension).Msg("Stream not found for detaching session")
		return
	}

	rtspStream.sessionMutex.Lock()
	rtspStream.session = nil
	rtspStream.sessionMutex.Unlock()

	log.Info().Str("extension", extension).Msg("Session detached from RTSP stream")
}

// RemoveStream removes a stream for an extension
func (rs *RTSPServer) RemoveStream(extension string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.removeStreamLocked(extension)
}

// removeStreamLocked removes a stream (caller must hold lock)
func (rs *RTSPServer) removeStreamLocked(extension string) {
	if rtspStream, exists := rs.streams[extension]; exists {
		if rtspStream.cancel != nil {
			rtspStream.cancel()
		}
		if rtspStream.stream != nil {
			rtspStream.stream.Close()
		}
		if rtspStream.streamWithBackchan != nil {
			rtspStream.streamWithBackchan.Close()
		}
		delete(rs.streams, extension)
		log.Info().Str("extension", extension).Msg("RTSP stream removed")
	}
}

// Stop stops the RTSP server
func (rs *RTSPServer) Stop() {
	rs.mu.Lock()
	// Stop all streams
	for extension := range rs.streams {
		rs.removeStreamLocked(extension)
	}
	rs.mu.Unlock()

	if rs.cancel != nil {
		rs.cancel()
	}
	if rs.server != nil {
		rs.server.Close()
	}
	log.Info().Msg("RTSP server stopped")
}

// OnConnOpen implements gortsplib.ServerHandlerOnConnOpen
func (rs *RTSPServer) OnConnOpen(ctx *gortsplib.ServerHandlerOnConnOpenCtx) {
	log.Info().Str("remote", ctx.Conn.NetConn().RemoteAddr().String()).Msg("RTSP connection opened")
}

// OnConnClose implements gortsplib.ServerHandlerOnConnClose
func (rs *RTSPServer) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	if ctx.Error != nil {
		log.Error().Err(ctx.Error).Msg("RTSP connection closed with error")
	} else {
		log.Info().Msg("RTSP connection closed")
	}
}

// OnRequest implements gortsplib.ServerHandlerOnRequest
func (rs *RTSPServer) OnRequest(conn *gortsplib.ServerConn, req *base.Request) {
	log.Debug().
		Str("method", string(req.Method)).
		Str("url", req.URL.String()).
		Msg("RTSP request received")
}

// OnResponse implements gortsplib.ServerHandlerOnResponse
func (rs *RTSPServer) OnResponse(conn *gortsplib.ServerConn, res *base.Response) {
	log.Debug().
		Str("status", fmt.Sprintf("%d %s", res.StatusCode, res.StatusMessage)).
		Msg("RTSP response sent")
}

// OnSessionOpen implements gortsplib.ServerHandlerOnSessionOpen
func (rs *RTSPServer) OnSessionOpen(ctx *gortsplib.ServerHandlerOnSessionOpenCtx) {
	log.Info().Msg("RTSP session opened")
}

// OnSessionClose implements gortsplib.ServerHandlerOnSessionClose
func (rs *RTSPServer) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	log.Info().Msg("RTSP session closed")
}

// OnDescribe implements gortsplib.ServerHandlerOnDescribe
func (rs *RTSPServer) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	log.Info().
		Str("path", ctx.Path).
		Str("query", ctx.Query).
		Str("remote", ctx.Conn.NetConn().RemoteAddr().String()).
		Msg("RTSP DESCRIBE request")

	// Extract extension from path (e.g., "/100" -> "100")
	extension := strings.TrimPrefix(ctx.Path, "/")

	rs.mu.RLock()
	rtspStream, exists := rs.streams[extension]
	rs.mu.RUnlock()

	if !exists {
		log.Warn().Str("extension", extension).Msg("Stream not found")
		return &base.Response{
			StatusCode: base.StatusNotFound,
		}, nil, nil
	}

	// Check for ONVIF backchannel requirement in headers
	backchannelRequested := false
	if ctx.Request != nil {
		if requireHeader := ctx.Request.Header["Require"]; requireHeader != nil {
			for _, val := range requireHeader {
				if strings.Contains(val, "www.onvif.org/ver20/backchannel") {
					backchannelRequested = true
					break
				}
			}
		}
	}

	// Store backchannel request state
	rtspStream.sessionMutex.Lock()
	rtspStream.backchannelRequested = backchannelRequested
	rtspStream.sessionMutex.Unlock()

	// Always return backchannel stream and advertise support
	if backchannelRequested {
		log.Info().
			Str("extension", extension).
			Bool("explicit_require", backchannelRequested).
			Msg("Returning 2-way RTSP stream with ONVIF backchannel support")

		response := &base.Response{
			StatusCode: base.StatusOK,
			Header: base.Header{
				"Supported": base.HeaderValue{"www.onvif.org/ver20/backchannel"},
			},
		}

		return response, rtspStream.streamWithBackchan, nil
	} else {
		log.Info().
			Str("extension", extension).
			Bool("explicit_require", backchannelRequested).
			Msg("Returning 1-way RTSP stream")

		response := &base.Response{
			StatusCode: base.StatusOK,
		}

		return response, rtspStream.stream, nil
	}
}

// OnSetup implements gortsplib.ServerHandlerOnSetup
func (rs *RTSPServer) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	log.Info().
		Str("path", ctx.Path).
		Str("transport", ctx.Transport.String()).
		Msg("RTSP SETUP request")

	// Extract extension from path
	extension := strings.TrimPrefix(ctx.Path, "/")

	rs.mu.RLock()
	rtspStream, exists := rs.streams[extension]
	rs.mu.RUnlock()

	if !exists {
		log.Error().Str("extension", extension).Msg("Stream not found for SETUP")
		return &base.Response{
			StatusCode: base.StatusNotFound,
		}, nil, fmt.Errorf("stream not found")
	}

	if rtspStream.backchannelRequested {
		return &base.Response{
			StatusCode: base.StatusOK,
		}, rtspStream.streamWithBackchan, nil
	} else {
		return &base.Response{
			StatusCode: base.StatusOK,
		}, rtspStream.stream, nil
	}
}

// OnPlay implements gortsplib.ServerHandlerOnPlay
func (rs *RTSPServer) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	log.Info().
		Str("path", ctx.Path).
		Str("session", fmt.Sprintf("%p", ctx.Session)).
		Msg("RTSP PLAY request")

	// Extract extension from path
	extension := strings.TrimPrefix(ctx.Path, "/")

	rs.mu.RLock()
	rtspStream, exists := rs.streams[extension]
	rs.mu.RUnlock()

	if exists && rtspStream.backchannelRequested {
		// Start backchannel receiver automatically for ONVIF support
		// The client can immediately start sending audio on the backchannel track
		rtspStream.sessionMutex.Lock()
		if !rtspStream.backchannelActive {
			rtspStream.backchannelActive = true
			rtspStream.sessionMutex.Unlock()

			log.Info().
				Str("extension", extension).
				Msg("Starting ONVIF backchannel receiver (automatic on PLAY)")

			go rs.receiveBackchannel(ctx.Session, rtspStream)
		} else {
			rtspStream.sessionMutex.Unlock()
		}
	}

	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// OnAnnounce implements gortsplib.ServerHandlerOnAnnounce
func (rs *RTSPServer) OnAnnounce(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	log.Info().Str("path", ctx.Path).Msg("RTSP ANNOUNCE request (not supported)")
	return &base.Response{
		StatusCode: base.StatusNotImplemented,
	}, fmt.Errorf("announce not supported")
}

// OnPause implements gortsplib.ServerHandlerOnPause
func (rs *RTSPServer) OnPause(ctx *gortsplib.ServerHandlerOnPauseCtx) (*base.Response, error) {
	log.Info().Str("path", ctx.Path).Msg("RTSP PAUSE request")
	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// OnGetParameter implements gortsplib.ServerHandlerOnGetParameter
func (rs *RTSPServer) OnGetParameter(ctx *gortsplib.ServerHandlerOnGetParameterCtx) (*base.Response, error) {
	log.Info().Str("path", ctx.Path).Msg("RTSP GET_PARAMETER request")
	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// OnSetParameter implements gortsplib.ServerHandlerOnSetParameter
func (rs *RTSPServer) OnSetParameter(ctx *gortsplib.ServerHandlerOnSetParameterCtx) (*base.Response, error) {
	log.Info().Str("path", ctx.Path).Msg("RTSP SET_PARAMETER request")
	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// receiveBackchannel receives RTP packets from RTSP client and forwards to SIP
func (rs *RTSPServer) receiveBackchannel(sess *gortsplib.ServerSession, rtspStream *RTSPStream) {
	// Add nil check for session
	if sess == nil {
		log.Error().Str("extension", rtspStream.extension).Msg("Cannot start backchannel receiver: session is nil")
		return
	}

	log.Info().
		Str("extension", rtspStream.extension).
		Str("session", fmt.Sprintf("%p", sess)).
		Msg("Starting backchannel receiver")

	// Get the backchannel media (second track in the backchannel stream)
	if len(rtspStream.streamWithBackchan.Description().Medias) < 2 {
		log.Error().Str("extension", rtspStream.extension).Msg("No backchannel media available")
		return
	}

	backchannelMedia := rtspStream.streamWithBackchan.Description().Medias[1]
	backchannelFormat := backchannelMedia.Formats[0]

	// Create a channel for receiving packets
	rtpChan := make(chan *rtp.Packet, 100)
	stopChan := make(chan struct{})

	// Set up packet reader with defer recovery to handle session close
	var packetCount uint64
	defer func() {
		if r := recover(); r != nil {
			log.Debug().
				Str("extension", rtspStream.extension).
				Interface("panic", r).
				Msg("Backchannel receiver recovered from panic (likely session closed)")
		}
		close(stopChan)
	}()

	sess.OnPacketRTP(backchannelMedia, backchannelFormat, func(pkt *rtp.Packet) {
		select {
		case rtpChan <- pkt:
			packetCount++
			if packetCount%50 == 0 {
				log.Debug().
					Str("extension", rtspStream.extension).
					Uint64("packets_received", packetCount).
					Uint16("seq", pkt.SequenceNumber).
					Int("payload_size", len(pkt.Payload)).
					Msg("Backchannel RTP packets received from RTSP client")
			}
		default:
			// Drop packet if channel is full
			log.Debug().Msg("Backchannel packet buffer full, dropping")
		}
	})

	// Process packets with timeout for graceful shutdown
	var sentCount uint64
	timeout := time.NewTimer(5 * time.Minute) // Max 5 minutes of inactivity
	defer timeout.Stop()

	for {
		select {
		case <-stopChan:
			log.Info().
				Str("extension", rtspStream.extension).
				Msg("Backchannel receiver stopped")
			return
		case <-timeout.C:
			log.Info().
				Str("extension", rtspStream.extension).
				Msg("Backchannel receiver timeout - stopping")
			return
		case pkt := <-rtpChan:
			// Reset timeout on activity
			if !timeout.Stop() {
				select {
				case <-timeout.C:
				default:
				}
			}
			timeout.Reset(5 * time.Minute)

			// Get the current session
			rtspStream.sessionMutex.RLock()
			session := rtspStream.session
			rtspStream.sessionMutex.RUnlock()

			if session == nil {
				log.Debug().Msg("No active SIP session for backchannel audio")
				continue
			}

			// Send the audio payload to the SIP caller via AudioBridge
			if session.audioBridge != nil {
				if err := session.audioBridge.SendToSIP(pkt.Payload); err != nil {
					log.Debug().Err(err).Msg("Failed to send backchannel audio to SIP")
				} else {
					sentCount++
					if sentCount == 1 {
						log.Info().
							Str("extension", rtspStream.extension).
							Str("call_id", session.callID).
							Msg("First backchannel packet successfully forwarded to SIP")
					}
				}
			}
		}
	}
}

// streamAudio reads audio from a session and streams it via RTP
func (rs *RTSPServer) streamAudio(ctx context.Context, rtspStream *RTSPStream) {
	ticker := time.NewTicker(20 * time.Millisecond) // 20ms frames
	defer ticker.Stop()

	sequenceNumber := uint16(0)
	timestamp := uint32(0)
	ssrc := uint32(0x12345678)

	silenceFrame := generateSilenceUlaw(ulawFrameSize)

	log.Info().Str("extension", rtspStream.extension).Msg("Audio streaming goroutine started")

	for {
		select {
		case <-ctx.Done():
			log.Info().Str("extension", rtspStream.extension).Msg("Audio streaming goroutine stopped")
			return
		case <-ticker.C:
			var audioFrame []byte

			// Get audio frame from session if one is attached
			rtspStream.sessionMutex.RLock()
			if rtspStream.session != nil {
				audioFrame = rtspStream.session.GetLatestAudioFrame()
			}
			rtspStream.sessionMutex.RUnlock()

			if audioFrame == nil {
				// No audio available or no session, use silence
				audioFrame = silenceFrame
			}

			// Create RTP packet
			packet := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    0, // PCMU
					SequenceNumber: sequenceNumber,
					Timestamp:      timestamp,
					SSRC:           ssrc,
					Marker:         false,
				},
				Payload: audioFrame,
			}

			// Write packet to both streams (standard and backchannel)
			// Standard stream (single track)
			if !rtspStream.backchannelRequested {
				err := rtspStream.stream.WritePacketRTP(rtspStream.stream.Description().Medias[0], packet)
				if err != nil {
					log.Debug().Err(err).Str("extension", rtspStream.extension).Msg("Failed to write RTP packet to standard stream")
				}
			}

			// Backchannel stream (first track - recvonly from client perspective)
			if rtspStream.backchannelRequested {
				err := rtspStream.streamWithBackchan.WritePacketRTP(rtspStream.streamWithBackchan.Description().Medias[0], packet)
				if err != nil {
					log.Debug().Err(err).Str("extension", rtspStream.extension).Msg("Failed to write RTP packet to backchannel stream")
				}
			}

			sequenceNumber++
			timestamp += uint32(ulawFrameSize) // 160 samples per frame at 8kHz
		}
	}
}

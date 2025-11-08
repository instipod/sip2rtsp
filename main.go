package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type CallSession struct {
	callID      string
	extension   string
	audioBridge *AudioBridge
	cancel      context.CancelFunc
	timeout     *time.Timer
	mu          sync.Mutex
}

// GetLatestAudioFrame returns the latest audio frame from this session
func (s *CallSession) GetLatestAudioFrame() []byte {
	if s.audioBridge == nil {
		return nil
	}
	return s.audioBridge.GetLatestFrame()
}

var (
	appConfig            *AppConfig
	sessions             = make(map[string]*CallSession) // indexed by callID
	sessionsMux          sync.RWMutex
	rtspServer           *RTSPServer
	registrationManagers = make(map[string]*RegistrationManager) // indexed by extension number
	sipUA                *sipgo.UserAgent
)

func main() {
	// Parse command line flags
	configFile := flag.String("config", "config.json", "Path to configuration file")
	createConfig := flag.Bool("create-config", false, "Create a default configuration file and exit")
	flag.Parse()

	// Setup logging (initial level)
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	// Handle config creation
	if *createConfig {
		if err := CreateDefaultConfig(*configFile); err != nil {
			log.Fatal().Err(err).Msg("Failed to create default config")
		}
		log.Info().Str("file", *configFile).Msg("Configuration file created successfully")
		return
	}

	// Load configuration
	var err error
	appConfig, err = LoadConfig(*configFile)
	if err != nil {
		log.Fatal().Err(err).Str("file", *configFile).Msg("Failed to load configuration")
	}

	// Set log level from config
	zerolog.SetGlobalLevel(appConfig.GetLogLevel())
	log.Info().Str("log_level", appConfig.LogLevel).Msg("Log level set")

	log.Info().
		Str("sip_addr", appConfig.SIPListenAddr).
		Int("sip_port", appConfig.SIPPort).
		Str("rtsp_addr", appConfig.RTSPListenAddr).
		Int("extensions", len(appConfig.Extensions)).
		Int("call_timeout_seconds", appConfig.CallTimeoutSeconds).
		Msg("Starting SIP to RTSP bridge")

	// Create and start RTSP server
	rtspServer, err = NewRTSPServer(appConfig.RTSPListenAddr)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to create RTSP server")
	}

	if err := rtspServer.Start(context.Background()); err != nil {
		log.Fatal().Err(err).Msg("Failed to start RTSP server")
	}

	// Initialize RTSP streams for all configured extensions (they will serve silence until a call arrives)
	for _, ext := range appConfig.Extensions {
		rtspServer.AddStaticStream(ext.Number)
		log.Info().
			Str("extension", ext.Number).
			Str("rtsp_path", "/"+ext.Number).
			Bool("one_call_only", ext.OneCallOnly).
			Msg("Initialized RTSP stream")
	}

	// Start SIP server
	if err := startSIPServer(); err != nil {
		log.Fatal().Err(err).Msg("Failed to start SIP server")
	}

	// Initialize SIP registrations for extensions that have it enabled
	registeredExts := appConfig.GetRegisteredExtensions()
	localIP := getLocalIP()
	for _, ext := range registeredExts {
		regMgr, err := NewRegistrationManager(ext.SIPRegistration, sipUA, localIP)
		if err != nil {
			log.Fatal().
				Err(err).
				Str("extension", ext.Number).
				Msg("Failed to create registration manager")
		}

		if err := regMgr.Start(context.Background()); err != nil {
			log.Fatal().
				Err(err).
				Str("extension", ext.Number).
				Msg("Failed to start SIP registration")
		}

		registrationManagers[ext.Number] = regMgr
		log.Info().
			Str("extension", ext.Number).
			Str("server", ext.SIPRegistration.Server).
			Str("username", ext.SIPRegistration.Username).
			Msg("SIP registration enabled for extension")
	}

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Info().Msg("Shutting down...")
	cleanup()
}

func startSIPServer() error {
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("SIP2RTSP/1.0"),
	)
	if err != nil {
		return fmt.Errorf("failed to create user agent: %w", err)
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	srv.OnInvite(handleInvite)
	srv.OnOptions(handleOptions)
	srv.OnBye(handleBye)

	addr := fmt.Sprintf("%s:%d", appConfig.SIPListenAddr, appConfig.SIPPort)
	go func() {
		if err := srv.ListenAndServe(context.Background(), "udp", addr); err != nil {
			log.Error().Err(err).Msg("SIP server error")
		}
	}()

	log.Info().Str("address", addr).Msg("SIP server listening")

	sipUA = ua

	return nil
}

func handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	log.Info().
		Str("from", req.From().Address.String()).
		Str("to", req.To().Address.String()).
		Str("call_id", req.CallID().Value()).
		Msg("Received INVITE")

	// Send 100 Trying
	res := sip.NewResponseFromRequest(req, 100, "Trying", nil)
	if err := tx.Respond(res); err != nil {
		log.Error().Err(err).Msg("Failed to send 100 Trying")
		return
	}

	// Parse SDP from INVITE
	sdpOffer := string(req.Body())
	if sdpOffer == "" {
		log.Error().Msg("No SDP in INVITE")
		respondError(req, tx, sip.StatusBadRequest, "Bad Request - No SDP")
		return
	}

	// Extract extension from To header (e.g., sip:100@domain -> "100")
	extension := req.To().Address.User
	if extension == "" {
		log.Error().Msg("No extension in To header")
		respondError(req, tx, sip.StatusBadRequest, "Bad Request - No extension")
		return
	}

	// Validate extension is in the configured list
	if !appConfig.IsValidExtension(extension) {
		log.Warn().Str("extension", extension).Msg("Extension not configured")
		respondError(req, tx, sip.StatusNotFound, "Not Found")
		return
	}

	// Check if extension is already in use (if one_call_only is enabled for this extension)
	extConfig := appConfig.GetExtension(extension)
	if extConfig.OneCallOnly {
		sessionsMux.RLock()
		for _, session := range sessions {
			if session.extension == extension {
				sessionsMux.RUnlock()
				log.Warn().Str("extension", extension).Msg("Extension already in use")
				respondError(req, tx, sip.StatusBusyHere, "Busy Here")
				return
			}
		}
		sessionsMux.RUnlock()
	}

	// Create call session and setup audio bridge
	session, err := setupCallSession(req.CallID().Value(), extension, sdpOffer)
	if err != nil {
		log.Error().Err(err).Msg("Failed to setup call session")
		respondError(req, tx, sip.StatusInternalServerError, "Internal Server Error")
		return
	}

	// Store session
	sessionsMux.Lock()
	sessions[req.CallID().Value()] = session
	sessionsMux.Unlock()

	// Attach session to RTSP stream
	rtspServer.AttachSession(extension, session)

	// Create SDP answer
	sdpAnswer := createSDPAnswer(req, session)

	// Send 200 OK
	res = sip.NewResponseFromRequest(req, 200, "OK", []byte(sdpAnswer))
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))

	if err := tx.Respond(res); err != nil {
		log.Error().Err(err).Msg("Failed to send 200 OK")
		return
	}

	log.Info().
		Str("call_id", req.CallID().Value()).
		Str("extension", extension).
		Str("rtsp_path", "/"+extension).
		Msg("Call established")
}

func handleBye(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID().Value()
	log.Info().Str("call_id", callID).Msg("Received BYE")

	// Clean up session
	cleanupSession(callID)

	// Send 200 OK
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	if err := tx.Respond(res); err != nil {
		log.Error().Err(err).Msg("Failed to send 200 OK for BYE")
	}
}

func handleOptions(req *sip.Request, tx sip.ServerTransaction) {
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	if err := tx.Respond(res); err != nil {
		log.Error().Err(err).Msg("Failed to send 200 OK for OPTIONS")
	}
}

func cleanupSession(callID string) {
	sessionsMux.Lock()
	defer sessionsMux.Unlock()

	session, exists := sessions[callID]
	if !exists {
		return
	}

	log.Info().
		Str("call_id", callID).
		Str("extension", session.extension).
		Msg("Cleaning up session")

	// Stop timeout timer if exists
	if session.timeout != nil {
		session.timeout.Stop()
	}

	// Cancel context
	session.cancel()

	// Stop audio bridge
	if session.audioBridge != nil {
		session.audioBridge.Stop()
	}

	// Detach session from RTSP server (stream will continue serving silence)
	rtspServer.DetachSession(session.extension)

	// Remove from sessions map
	delete(sessions, callID)
}

func setupCallSession(callID string, extension string, sdpOffer string) (*CallSession, error) {
	ctx, cancel := context.WithCancel(context.Background())

	session := &CallSession{
		callID:    callID,
		extension: extension,
		cancel:    cancel,
	}

	// Create audio bridge
	audioBridge, err := NewAudioBridge(session)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create audio bridge: %w", err)
	}
	session.audioBridge = audioBridge

	// Start audio bridge
	if err := audioBridge.Start(ctx); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to start audio bridge: %w", err)
	}

	// Setup timeout if configured
	timeout := appConfig.GetCallTimeout()
	if timeout > 0 {
		session.timeout = time.AfterFunc(timeout, func() {
			log.Warn().
				Str("call_id", callID).
				Str("extension", extension).
				Dur("timeout", timeout).
				Msg("Call timeout reached, terminating call")
			cleanupSession(callID)
		})
		log.Info().
			Str("call_id", callID).
			Dur("timeout", timeout).
			Msg("Call timeout set")
	}

	return session, nil
}

func createSDPAnswer(req *sip.Request, session *CallSession) string {
	// Get the RTP port from the audio bridge
	rtpPort := session.audioBridge.GetLocalPort()

	// Get local IP (simplified - should use proper detection)
	localIP := appConfig.SIPListenAddr
	if localIP == "0.0.0.0" {
		localIP = getLocalIP()
	}

	sdp := fmt.Sprintf(`v=0
o=- %d 0 IN IP4 %s
s=SIP2WebRTC
c=IN IP4 %s
t=0 0
m=audio %d RTP/AVP 0
a=rtpmap:0 PCMU/8000
a=sendrecv
`, time.Now().Unix(), localIP, localIP, rtpPort)

	return sdp
}

// getLocalIP attempts to get the local IP address
func getLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}

func respondError(req *sip.Request, tx sip.ServerTransaction, code sip.StatusCode, reason string) {
	res := sip.NewResponseFromRequest(req, code, reason, nil)
	if err := tx.Respond(res); err != nil {
		log.Error().Err(err).Msg("Failed to send error response")
	}
}

func cleanup() {
	// Stop all SIP registrations
	for ext, regMgr := range registrationManagers {
		log.Info().Str("extension", ext).Msg("Stopping SIP registration")
		regMgr.Stop()
	}

	// Stop RTSP server
	if rtspServer != nil {
		rtspServer.Stop()
	}

	// Clean up sessions
	sessionsMux.Lock()
	defer sessionsMux.Unlock()

	for _, session := range sessions {
		session.cancel()
		if session.audioBridge != nil {
			session.audioBridge.Stop()
		}
	}
}

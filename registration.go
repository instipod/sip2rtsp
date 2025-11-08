package main

import (
	"context"
	"crypto/md5"
	"fmt"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog/log"
)

type RegistrationManager struct {
	config *SIPRegistration
	client *sipgo.Client
	dialog *sipgo.DialogClientSession
	cancel context.CancelFunc
}

// NewRegistrationManager creates a new SIP registration manager
func NewRegistrationManager(config *SIPRegistration) (*RegistrationManager, error) {
	if !config.Enabled {
		return nil, nil
	}

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("SIP2RTSP/1.0"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create user agent: %w", err)
	}

	client, err := sipgo.NewClient(ua)
	if err != nil {
		return nil, fmt.Errorf("failed to create SIP client: %w", err)
	}

	return &RegistrationManager{
		config: config,
		client: client,
	}, nil
}

// Start begins SIP registration and maintains it
func (rm *RegistrationManager) Start(ctx context.Context) error {
	if rm == nil {
		return nil
	}

	ctx, rm.cancel = context.WithCancel(ctx)

	// Perform initial registration
	if err := rm.register(ctx); err != nil {
		return fmt.Errorf("initial registration failed: %w", err)
	}

	// Start re-registration loop
	go rm.reregisterLoop(ctx)

	return nil
}

// Stop stops the registration manager
func (rm *RegistrationManager) Stop() {
	if rm == nil {
		return
	}

	log.Info().Msg("Stopping SIP registration")

	if rm.cancel != nil {
		rm.cancel()
	}

	// Send unregister (expires=0)
	if rm.dialog != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := rm.unregister(ctx); err != nil {
			log.Error().Err(err).Msg("Failed to unregister")
		}
	}
}

// register performs SIP registration
func (rm *RegistrationManager) register(ctx context.Context) error {
	log.Info().
		Str("server", rm.config.Server).
		Str("username", rm.config.Username).
		Int("expires", rm.config.Expires).
		Msg("Registering with SIP server")

	recipient := &sip.Uri{
		User: rm.config.Username,
		Host: rm.config.Server,
	}

	from := &sip.Uri{
		User: rm.config.Username,
		Host: rm.config.Server,
	}

	req := sip.NewRequest(sip.REGISTER, recipient)
	req.SetDestination(rm.config.Server)
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<%s>;tag=%d", from.String(), time.Now().Unix())))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<%s>", from.String())))
	req.AppendHeader(sip.NewHeader("Contact", fmt.Sprintf("<%s>", from.String())))
	req.AppendHeader(sip.NewHeader("Expires", fmt.Sprintf("%d", rm.config.Expires)))

	cseq := sip.CSeqHeader{
		SeqNo:      1,
		MethodName: sip.REGISTER,
	}
	req.AppendHeader(&cseq)

	callID := sip.CallIDHeader(fmt.Sprintf("%d@%s", time.Now().UnixNano(), from.Host))
	req.AppendHeader(&callID)

	// Send REGISTER
	tx, err := rm.client.TransactionRequest(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to send REGISTER: %w", err)
	}

	// Wait for response
	select {
	case res := <-tx.Responses():
		if res == nil {
			return fmt.Errorf("no response received")
		}

		log.Info().
			Int("status_code", int(res.StatusCode)).
			Str("status", res.Reason).
			Msg("Registration response received")

		// Handle authentication challenge
		if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
			return rm.registerWithAuth(ctx, req, res)
		}

		if res.StatusCode != sip.StatusOK {
			return fmt.Errorf("registration failed: %d %s", res.StatusCode, res.Reason)
		}

		log.Info().Msg("Successfully registered with SIP server")
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// registerWithAuth performs registration with authentication
func (rm *RegistrationManager) registerWithAuth(ctx context.Context, originalReq *sip.Request, challengeRes *sip.Response) error {
	log.Debug().Msg("Handling authentication challenge")

	// Get WWW-Authenticate or Proxy-Authenticate header
	var authHeader sip.Header
	if h := challengeRes.GetHeader("WWW-Authenticate"); h != nil {
		authHeader = h
	} else if h := challengeRes.GetHeader("Proxy-Authenticate"); h != nil {
		authHeader = h
	} else {
		return fmt.Errorf("no authentication challenge found in response")
	}

	// Parse authentication parameters
	authParams := parseAuthHeader(authHeader.Value())

	// Calculate response
	auth := calculateDigestResponse(
		rm.config.Username,
		rm.config.Password,
		originalReq.Method.String(),
		originalReq.Recipient.String(),
		authParams,
	)

	// Create new request with Authorization header
	req := originalReq.Clone()

	// Update CSeq
	if cseq := req.CSeq(); cseq != nil {
		cseq.SeqNo++
	}

	// Add Authorization header
	authHeaderValue := fmt.Sprintf(
		`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", algorithm=MD5`,
		rm.config.Username,
		authParams["realm"],
		authParams["nonce"],
		originalReq.Recipient.String(),
		auth,
	)
	req.AppendHeader(sip.NewHeader("Authorization", authHeaderValue))

	// Send authenticated REGISTER
	tx, err := rm.client.TransactionRequest(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to send authenticated REGISTER: %w", err)
	}

	// Wait for response
	select {
	case res := <-tx.Responses():
		if res == nil {
			return fmt.Errorf("no response received")
		}

		log.Info().
			Int("status_code", int(res.StatusCode)).
			Str("status", res.Reason).
			Msg("Authenticated registration response received")

		if res.StatusCode != sip.StatusOK {
			return fmt.Errorf("authenticated registration failed: %d %s", res.StatusCode, res.Reason)
		}

		log.Info().Msg("Successfully registered with SIP server (authenticated)")
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// unregister sends unregister request (expires=0)
func (rm *RegistrationManager) unregister(ctx context.Context) error {
	log.Info().Msg("Unregistering from SIP server")

	recipient := &sip.Uri{
		User: rm.config.Username,
		Host: rm.config.Server,
	}

	from := &sip.Uri{
		User: rm.config.Username,
		Host: rm.config.Server,
	}

	req := sip.NewRequest(sip.REGISTER, recipient)
	req.SetDestination(rm.config.Server)
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<%s>;tag=%d", from.String(), time.Now().Unix())))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<%s>", from.String())))
	req.AppendHeader(sip.NewHeader("Contact", fmt.Sprintf("<%s>", from.String())))
	req.AppendHeader(sip.NewHeader("Expires", "0")) // Unregister

	cseq := sip.CSeqHeader{
		SeqNo:      2,
		MethodName: sip.REGISTER,
	}
	req.AppendHeader(&cseq)

	callID := sip.CallIDHeader(fmt.Sprintf("%d@%s", time.Now().UnixNano(), from.Host))
	req.AppendHeader(&callID)

	tx, err := rm.client.TransactionRequest(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to send unregister: %w", err)
	}

	select {
	case res := <-tx.Responses():
		if res != nil && res.StatusCode == sip.StatusOK {
			log.Info().Msg("Successfully unregistered")
			return nil
		}
		return fmt.Errorf("unregister failed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// reregisterLoop periodically re-registers before expiration
func (rm *RegistrationManager) reregisterLoop(ctx context.Context) {
	// Re-register at 80% of expiration time
	interval := time.Duration(float64(rm.config.Expires)*0.8) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Info().Dur("interval", interval).Msg("Starting re-registration loop")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := rm.register(ctx); err != nil {
				log.Error().Err(err).Msg("Re-registration failed")
			}
		}
	}
}

// Helper functions for digest authentication
func parseAuthHeader(header string) map[string]string {
	params := make(map[string]string)

	// Remove "Digest " prefix
	header = strings.TrimPrefix(header, "Digest ")

	// Split by comma
	parts := strings.Split(header, ",")

	for _, part := range parts {
		part = strings.TrimSpace(part)
		// Split by equals
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}

		key := strings.TrimSpace(kv[0])
		value := strings.Trim(strings.TrimSpace(kv[1]), "\"")
		params[key] = value
	}

	return params
}

func calculateDigestResponse(username, password, method, uri string, params map[string]string) string {
	realm := params["realm"]
	nonce := params["nonce"]

	// Calculate HA1 = MD5(username:realm:password)
	ha1 := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%s:%s:%s", username, realm, password))))

	// Calculate HA2 = MD5(method:uri)
	ha2 := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%s:%s", method, uri))))

	// Calculate response = MD5(HA1:nonce:HA2)
	response := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%s:%s:%s", ha1, nonce, ha2))))

	return response
}

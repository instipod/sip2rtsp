package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type SIPRegistration struct {
	Enabled     bool   `json:"enabled"`
	Server      string `json:"server"`       // SIP server address (e.g., "sip.example.com:5060")
	Username    string `json:"username"`     // SIP username for this extension
	Password    string `json:"password"`     // SIP password for this extension
	DisplayName string `json:"display_name"` // Display name (optional)
	Expires     int    `json:"expires"`      // Registration expires in seconds (default: 3600)
}

type Extension struct {
	Number          string           `json:"number"`                     // Extension number (e.g., "100")
	OneCallOnly     bool             `json:"one_call_only"`              // Only allow one call at a time for this extension
	SIPRegistration *SIPRegistration `json:"sip_registration,omitempty"` // SIP registration for this extension
}

type AppConfig struct {
	SIPListenAddr      string       `json:"sip_listen_addr"`
	SIPPort            int          `json:"sip_port"`
	RTSPListenAddr     string       `json:"rtsp_listen_addr"`
	LogLevel           string       `json:"log_level"` // debug, info, warn, error
	Extensions         []*Extension `json:"extensions"`
	CallTimeoutSeconds int          `json:"call_timeout_seconds"` // 0 means no timeout (applies to all extensions)
}

// LoadConfig loads configuration from a JSON file
func LoadConfig(filename string) (*AppConfig, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config AppConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Set defaults
	if config.SIPListenAddr == "" {
		config.SIPListenAddr = "0.0.0.0"
	}
	if config.SIPPort == 0 {
		config.SIPPort = 5060
	}
	if config.RTSPListenAddr == "" {
		config.RTSPListenAddr = ":9554"
	}
	if config.LogLevel == "" {
		config.LogLevel = "info"
	}

	// Set defaults for each extension
	for _, ext := range config.Extensions {
		if ext.SIPRegistration != nil && ext.SIPRegistration.Enabled {
			if ext.SIPRegistration.Expires == 0 {
				ext.SIPRegistration.Expires = 3600
			}
		}
	}

	return &config, nil
}

// GetCallTimeout returns the call timeout duration, or 0 if no timeout
func (c *AppConfig) GetCallTimeout() time.Duration {
	if c.CallTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(c.CallTimeoutSeconds) * time.Second
}

// GetLogLevel returns the zerolog level for the configured log level
func (c *AppConfig) GetLogLevel() zerolog.Level {
	switch c.LogLevel {
	case "debug":
		return zerolog.DebugLevel
	case "info":
		return zerolog.InfoLevel
	case "warn":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	default:
		return zerolog.InfoLevel
	}
}

// IsValidExtension checks if an extension is in the allowed list
func (c *AppConfig) IsValidExtension(extension string) bool {
	for _, ext := range c.Extensions {
		if ext.Number == extension {
			return true
		}
	}
	return false
}

// GetExtension returns the extension configuration for a given number
func (c *AppConfig) GetExtension(number string) *Extension {
	for _, ext := range c.Extensions {
		if ext.Number == number {
			return ext
		}
	}
	return nil
}

// GetRegisteredExtensions returns all extensions that have registration enabled
func (c *AppConfig) GetRegisteredExtensions() []*Extension {
	var registered []*Extension
	for _, ext := range c.Extensions {
		if ext.SIPRegistration != nil && ext.SIPRegistration.Enabled {
			registered = append(registered, ext)
		}
	}
	return registered
}

// CreateDefaultConfig creates a default configuration file
func CreateDefaultConfig(filename string) error {
	config := AppConfig{
		SIPListenAddr:      "0.0.0.0",
		SIPPort:            5060,
		RTSPListenAddr:     ":9554",
		LogLevel:           "info",
		CallTimeoutSeconds: 3600, // 1 hour default
		Extensions: []*Extension{
			{
				Number:      "100",
				OneCallOnly: true,
				SIPRegistration: &SIPRegistration{
					Enabled:     false,
					Server:      "sip.example.com:5060",
					Username:    "100",
					Password:    "password100",
					DisplayName: "Extension 100",
					Expires:     3600,
				},
			},
			{
				Number:      "101",
				OneCallOnly: true,
				SIPRegistration: &SIPRegistration{
					Enabled:     false,
					Server:      "sip.example.com:5060",
					Username:    "101",
					Password:    "password101",
					DisplayName: "Extension 101",
					Expires:     3600,
				},
			},
		},
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	log.Info().Str("filename", filename).Msg("Created default configuration file")
	return nil
}

package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var apiKeyRegex = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ChannelNameRegex matches valid channel names per PRD 3.1.6:
// 1-128 chars, [a-zA-Z0-9_./-], no consecutive dots, no leading/trailing dots.
var ChannelNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_/-](?:\.?[a-zA-Z0-9_/-]){0,127}$`)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Database  DatabaseConfig  `yaml:"database"`
	Auth      AuthConfig      `yaml:"auth"`
	WebSocket WebSocketConfig `yaml:"websocket"`
	Retention RetentionConfig `yaml:"retention"`
	Shutdown  ShutdownConfig  `yaml:"shutdown"`
	Log       LogConfig       `yaml:"log"`
}

type ServerConfig struct {
	Addr           string `yaml:"addr"`
	TLSCert        string `yaml:"tls_cert"`
	TLSKey         string `yaml:"tls_key"`
	MaxPayloadSize int    `yaml:"max_payload_size"`
}

type DatabaseConfig struct {
	DSN             string        `yaml:"dsn"`
	MaxOpenConns    int           `yaml:"max_open_conns"`
	MaxIdleConns    int           `yaml:"max_idle_conns"`
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
}

type APIKeyEntry struct {
	Key         string `yaml:"key"`
	Description string `yaml:"description"`
}

type AuthConfig struct {
	JWTSigningKey string        `yaml:"jwt_signing_key"`
	JWTClockSkew  time.Duration `yaml:"jwt_clock_skew"`
	APIKeys       []APIKeyEntry `yaml:"api_keys"`
}

type WebSocketConfig struct {
	PingInterval   time.Duration `yaml:"ping_interval"`
	PongTimeout    time.Duration `yaml:"pong_timeout"`
	OutboundBuffer int           `yaml:"outbound_buffer"`
	MaxMessageSize int           `yaml:"max_message_size"`
	AllowedOrigins []string      `yaml:"allowed_origins"`
}

type RetentionRule struct {
	Pattern  string        `yaml:"pattern"`
	TTL      time.Duration `yaml:"ttl"`
	MaxCount int           `yaml:"max_count"`
}

type RetentionConfig struct {
	DefaultTTL      time.Duration  `yaml:"default_ttl"`
	DefaultMaxCount int            `yaml:"default_max_count"`
	EvictionInterval time.Duration `yaml:"eviction_interval"`
	Rules           []RetentionRule `yaml:"rules"`
}

type ShutdownConfig struct {
	Timeout time.Duration `yaml:"timeout"`
}

type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

func defaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Addr:           ":8080",
			MaxPayloadSize: 65536,
		},
		Database: DatabaseConfig{
			MaxOpenConns:    25,
			MaxIdleConns:    10,
			ConnMaxIdleTime: 5 * time.Minute,
			ConnMaxLifetime: 30 * time.Minute,
		},
		Auth: AuthConfig{
			JWTClockSkew: 30 * time.Second,
		},
		WebSocket: WebSocketConfig{
			PingInterval:   30 * time.Second,
			PongTimeout:    60 * time.Second,
			OutboundBuffer: 256,
			MaxMessageSize: 65536,
		},
		Retention: RetentionConfig{
			DefaultTTL:      720 * time.Hour,
			DefaultMaxCount: 10000,
			EvictionInterval: 5 * time.Minute,
		},
		Shutdown: ShutdownConfig{
			Timeout: 10 * time.Second,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
	}
}

func Load(path string) (*Config, error) {
	if path == "" {
		return nil, fmt.Errorf("config file path is required")
	}

	cfg := defaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config file: %w", err)
	}

	if err := applyEnvOverrides(cfg); err != nil {
		return nil, fmt.Errorf("apply environment overrides: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func applyEnvOverrides(cfg *Config) error {
	overrides := []struct {
		env    string
		target interface{}
		kind   string // "string", "int", "bool", "duration"
	}{
		// server
		{"AETHER_SERVER_ADDR", &cfg.Server.Addr, "string"},
		{"AETHER_SERVER_TLS_CERT", &cfg.Server.TLSCert, "string"},
		{"AETHER_SERVER_TLS_KEY", &cfg.Server.TLSKey, "string"},
		{"AETHER_SERVER_MAX_PAYLOAD_SIZE", &cfg.Server.MaxPayloadSize, "int"},
		// database
		{"AETHER_DATABASE_DSN", &cfg.Database.DSN, "string"},
		{"AETHER_DATABASE_MAX_OPEN_CONNS", &cfg.Database.MaxOpenConns, "int"},
		{"AETHER_DATABASE_MAX_IDLE_CONNS", &cfg.Database.MaxIdleConns, "int"},
		{"AETHER_DATABASE_CONN_MAX_IDLE_TIME", &cfg.Database.ConnMaxIdleTime, "duration"},
		{"AETHER_DATABASE_CONN_MAX_LIFETIME", &cfg.Database.ConnMaxLifetime, "duration"},
		// auth
		{"AETHER_AUTH_JWT_SIGNING_KEY", &cfg.Auth.JWTSigningKey, "string"},
		{"AETHER_AUTH_JWT_CLOCK_SKEW", &cfg.Auth.JWTClockSkew, "duration"},
		// websocket
		{"AETHER_WEBSOCKET_PING_INTERVAL", &cfg.WebSocket.PingInterval, "duration"},
		{"AETHER_WEBSOCKET_PONG_TIMEOUT", &cfg.WebSocket.PongTimeout, "duration"},
		{"AETHER_WEBSOCKET_OUTBOUND_BUFFER", &cfg.WebSocket.OutboundBuffer, "int"},
		{"AETHER_WEBSOCKET_MAX_MESSAGE_SIZE", &cfg.WebSocket.MaxMessageSize, "int"},
		// retention
		{"AETHER_RETENTION_DEFAULT_TTL", &cfg.Retention.DefaultTTL, "duration"},
		{"AETHER_RETENTION_DEFAULT_MAX_COUNT", &cfg.Retention.DefaultMaxCount, "int"},
		{"AETHER_RETENTION_EVICTION_INTERVAL", &cfg.Retention.EvictionInterval, "duration"},
		// shutdown
		{"AETHER_SHUTDOWN_TIMEOUT", &cfg.Shutdown.Timeout, "duration"},
		// log
		{"AETHER_LOG_LEVEL", &cfg.Log.Level, "string"},
		{"AETHER_LOG_FORMAT", &cfg.Log.Format, "string"},
	}

	for _, o := range overrides {
		val, ok := os.LookupEnv(o.env)
		if !ok {
			continue
		}
		switch o.kind {
		case "string":
			*(o.target.(*string)) = val
		case "int":
			v, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("invalid int for %s: %w", o.env, err)
			}
			*(o.target.(*int)) = v
		case "duration":
			v, err := time.ParseDuration(val)
			if err != nil {
				return fmt.Errorf("invalid duration for %s: %w", o.env, err)
			}
			*(o.target.(*time.Duration)) = v
		}
	}

	return nil
}

func (c *Config) Validate() error {
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required")
	}
	if c.Database.MaxOpenConns <= 0 {
		return fmt.Errorf("database.max_open_conns must be positive")
	}
	if c.Database.MaxIdleConns <= 0 {
		return fmt.Errorf("database.max_idle_conns must be positive")
	}
	if c.Database.ConnMaxIdleTime <= 0 {
		return fmt.Errorf("database.conn_max_idle_time must be positive")
	}
	if c.Database.ConnMaxLifetime <= 0 {
		return fmt.Errorf("database.conn_max_lifetime must be positive")
	}

	if c.Auth.JWTSigningKey == "" {
		return fmt.Errorf("auth.jwt_signing_key is required")
	}
	if len(c.Auth.JWTSigningKey) < 32 {
		return fmt.Errorf("auth.jwt_signing_key must be at least 32 bytes")
	}
	for i, ak := range c.Auth.APIKeys {
		if len(ak.Key) < 43 {
			return fmt.Errorf("auth.api_keys[%d].key must be at least 43 characters (base64url-encoded 32 bytes)", i)
		}
		if !apiKeyRegex.MatchString(ak.Key) {
			return fmt.Errorf("auth.api_keys[%d].key contains invalid characters (only [A-Za-z0-9_-] allowed)", i)
		}
	}

	if c.WebSocket.PingInterval <= 0 {
		return fmt.Errorf("websocket.ping_interval must be positive")
	}
	if c.WebSocket.OutboundBuffer <= 0 {
		return fmt.Errorf("websocket.outbound_buffer must be positive")
	}
	if c.WebSocket.MaxMessageSize <= 0 {
		return fmt.Errorf("websocket.max_message_size must be positive")
	}
	if c.WebSocket.PongTimeout < c.WebSocket.PingInterval {
		return fmt.Errorf("websocket.pong_timeout must be >= websocket.ping_interval")
	}
	for _, origin := range c.WebSocket.AllowedOrigins {
		if origin != "*" && !strings.HasPrefix(origin, "http://") && !strings.HasPrefix(origin, "https://") {
			return fmt.Errorf("websocket.allowed_origins: invalid origin %q (must be \"*\" or start with http:// or https://)", origin)
		}
	}

	if c.Retention.DefaultTTL <= 0 {
		return fmt.Errorf("retention.default_ttl must be positive")
	}
	if c.Retention.DefaultMaxCount <= 0 {
		return fmt.Errorf("retention.default_max_count must be positive")
	}
	if c.Retention.EvictionInterval <= 0 {
		return fmt.Errorf("retention.eviction_interval must be positive")
	}
	for i, rule := range c.Retention.Rules {
		if rule.Pattern == "" {
			return fmt.Errorf("retention.rules[%d].pattern is required", i)
		}
		if err := validateRetentionPattern(rule.Pattern); err != nil {
			return fmt.Errorf("retention.rules[%d].pattern: %w", i, err)
		}
		if rule.TTL <= 0 {
			return fmt.Errorf("retention.rules[%d].ttl must be positive", i)
		}
		if rule.MaxCount <= 0 {
			return fmt.Errorf("retention.rules[%d].max_count must be positive", i)
		}
	}

	if c.Shutdown.Timeout <= 0 {
		return fmt.Errorf("shutdown.timeout must be positive")
	}

	if c.Server.MaxPayloadSize <= 0 {
		return fmt.Errorf("server.max_payload_size must be positive")
	}

	if c.Server.TLSCert != "" && c.Server.TLSKey == "" {
		return fmt.Errorf("server.tls_key is required when server.tls_cert is set")
	}
	if c.Server.TLSKey != "" && c.Server.TLSCert == "" {
		return fmt.Errorf("server.tls_cert is required when server.tls_key is set")
	}

	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[c.Log.Level] {
		return fmt.Errorf("log.level must be one of debug, info, warn, error")
	}

	validLogFormats := map[string]bool{"json": true, "text": true}
	if !validLogFormats[c.Log.Format] {
		return fmt.Errorf("log.format must be one of json, text")
	}

	return nil
}

func validateRetentionPattern(pattern string) error {
	if strings.HasSuffix(pattern, ".*") {
		prefix := strings.TrimSuffix(pattern, ".*")
		if prefix == "" || !ChannelNameRegex.MatchString(prefix) {
			return fmt.Errorf("prefix %q before .* must be a valid channel name segment", prefix)
		}
	} else {
		if !ChannelNameRegex.MatchString(pattern) {
			return fmt.Errorf("%q is not a valid channel name or prefix.* pattern", pattern)
		}
	}
	return nil
}

// MatchRetentionRule returns the first retention rule whose pattern matches the channel name.
// Pattern "prefix.*" matches all channels starting with "prefix.".
func (c *Config) MatchRetentionRule(channel string) (ttl time.Duration, maxCount int) {
	for _, rule := range c.Retention.Rules {
		if strings.HasSuffix(rule.Pattern, ".*") {
			prefix := strings.TrimSuffix(rule.Pattern, ".*")
			if strings.HasPrefix(channel, prefix+".") {
				return rule.TTL, rule.MaxCount
			}
		} else {
			if channel == rule.Pattern {
				return rule.TTL, rule.MaxCount
			}
		}
	}
	return c.Retention.DefaultTTL, c.Retention.DefaultMaxCount
}

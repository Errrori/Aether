package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return path
}

func TestLoad_Defaults(t *testing.T) {
	path := writeTestConfig(t, `
database:
  dsn: "postgres://localhost/aether"
auth:
  jwt_signing_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Addr != ":8080" {
		t.Errorf("Server.Addr = %q, want :8080", cfg.Server.Addr)
	}
	if cfg.Database.MaxOpenConns != 25 {
		t.Errorf("Database.MaxOpenConns = %d, want 25", cfg.Database.MaxOpenConns)
	}
	if cfg.Database.MaxIdleConns != 10 {
		t.Errorf("Database.MaxIdleConns = %d, want 10", cfg.Database.MaxIdleConns)
	}
	if cfg.Database.ConnMaxIdleTime != 5*time.Minute {
		t.Errorf("Database.ConnMaxIdleTime = %v, want 5m", cfg.Database.ConnMaxIdleTime)
	}
	if cfg.Database.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("Database.ConnMaxLifetime = %v, want 30m", cfg.Database.ConnMaxLifetime)
	}
	if cfg.Auth.JWTClockSkew != 30*time.Second {
		t.Errorf("Auth.JWTClockSkew = %v, want 30s", cfg.Auth.JWTClockSkew)
	}
	if cfg.WebSocket.PingInterval != 30*time.Second {
		t.Errorf("WebSocket.PingInterval = %v, want 30s", cfg.WebSocket.PingInterval)
	}
	if cfg.WebSocket.PongTimeout != 60*time.Second {
		t.Errorf("WebSocket.PongTimeout = %v, want 60s", cfg.WebSocket.PongTimeout)
	}
	if cfg.WebSocket.OutboundBuffer != 256 {
		t.Errorf("WebSocket.OutboundBuffer = %d, want 256", cfg.WebSocket.OutboundBuffer)
	}
	if cfg.WebSocket.MaxMessageSize != 65536 {
		t.Errorf("WebSocket.MaxMessageSize = %d, want 65536", cfg.WebSocket.MaxMessageSize)
	}
	if cfg.Retention.DefaultTTL != 720*time.Hour {
		t.Errorf("Retention.DefaultTTL = %v, want 720h", cfg.Retention.DefaultTTL)
	}
	if cfg.Retention.DefaultMaxCount != 10000 {
		t.Errorf("Retention.DefaultMaxCount = %d, want 10000", cfg.Retention.DefaultMaxCount)
	}
	if cfg.Retention.EvictionInterval != 5*time.Minute {
		t.Errorf("Retention.EvictionInterval = %v, want 5m", cfg.Retention.EvictionInterval)
	}
	if cfg.Shutdown.Timeout != 10*time.Second {
		t.Errorf("Shutdown.Timeout = %v, want 10s", cfg.Shutdown.Timeout)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want info", cfg.Log.Level)
	}
	if cfg.Log.Format != "json" {
		t.Errorf("Log.Format = %q, want json", cfg.Log.Format)
	}
}

func TestLoad_MissingRequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "missing dsn",
			yaml: `auth:
  jwt_signing_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`,
			wantErr: "database.dsn is required",
		},
		{
			name: "missing jwt_signing_key",
			yaml: `database:
  dsn: "postgres://localhost/aether"`,
			wantErr: "auth.jwt_signing_key is required",
		},
		{
			name: "jwt_signing_key too short",
			yaml: `database:
  dsn: "postgres://localhost/aether"
auth:
  jwt_signing_key: "short"`,
			wantErr: "auth.jwt_signing_key must be at least 32 bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTestConfig(t, tt.yaml)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestLoad_EmptyPath(t *testing.T) {
	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "config file path is required") {
		t.Errorf("expected error about empty path, got %v", err)
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	path := writeTestConfig(t, `
database:
  dsn: "postgres://localhost/aether"
auth:
  jwt_signing_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`)

	t.Setenv("AETHER_DATABASE_DSN", "postgres://overridden/aether")
	t.Setenv("AETHER_SERVER_ADDR", ":9090")
	t.Setenv("AETHER_LOG_LEVEL", "debug")
	t.Setenv("AETHER_WEBSOCKET_OUTBOUND_BUFFER", "512")
	t.Setenv("AETHER_WEBSOCKET_PING_INTERVAL", "15s")
	t.Setenv("AETHER_AUTH_JWT_CLOCK_SKEW", "1m")
	t.Setenv("AETHER_RETENTION_DEFAULT_MAX_COUNT", "5000")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Database.DSN != "postgres://overridden/aether" {
		t.Errorf("DSN not overridden, got %q", cfg.Database.DSN)
	}
	if cfg.Server.Addr != ":9090" {
		t.Errorf("Addr not overridden, got %q", cfg.Server.Addr)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level not overridden, got %q", cfg.Log.Level)
	}
	if cfg.WebSocket.OutboundBuffer != 512 {
		t.Errorf("OutboundBuffer not overridden, got %d", cfg.WebSocket.OutboundBuffer)
	}
	if cfg.WebSocket.PingInterval != 15*time.Second {
		t.Errorf("PingInterval not overridden, got %v", cfg.WebSocket.PingInterval)
	}
	if cfg.Auth.JWTClockSkew != 1*time.Minute {
		t.Errorf("JWTClockSkew not overridden, got %v", cfg.Auth.JWTClockSkew)
	}
	if cfg.Retention.DefaultMaxCount != 5000 {
		t.Errorf("DefaultMaxCount not overridden, got %d", cfg.Retention.DefaultMaxCount)
	}
}

func TestLoad_EnvProvidesRequired(t *testing.T) {
	path := writeTestConfig(t, `
server:
  addr: ":8080"
`)

	t.Setenv("AETHER_DATABASE_DSN", "postgres://env/aether")
	t.Setenv("AETHER_AUTH_JWT_SIGNING_KEY", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.DSN != "postgres://env/aether" {
		t.Errorf("DSN = %q, want from env", cfg.Database.DSN)
	}
	if cfg.Auth.JWTSigningKey != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("JWTSigningKey not set from env")
	}
}

func TestLoad_EnvOverrideInvalid(t *testing.T) {
	path := writeTestConfig(t, `
database:
  dsn: "postgres://localhost/aether"
auth:
  jwt_signing_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`)

	t.Setenv("AETHER_WEBSOCKET_OUTBOUND_BUFFER", "notanumber")

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid int env var")
	}
}

func TestValidate_PositiveConstraints(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr string
	}{
		{
			name:    "max_open_conns zero",
			modify:  func(c *Config) { c.Database.MaxOpenConns = 0 },
			wantErr: "database.max_open_conns must be positive",
		},
		{
			name:    "max_idle_conns zero",
			modify:  func(c *Config) { c.Database.MaxIdleConns = 0 },
			wantErr: "database.max_idle_conns must be positive",
		},
		{
			name:    "conn_max_idle_time zero",
			modify:  func(c *Config) { c.Database.ConnMaxIdleTime = 0 },
			wantErr: "database.conn_max_idle_time must be positive",
		},
		{
			name:    "conn_max_lifetime zero",
			modify:  func(c *Config) { c.Database.ConnMaxLifetime = 0 },
			wantErr: "database.conn_max_lifetime must be positive",
		},
		{
			name:    "ping_interval zero",
			modify:  func(c *Config) { c.WebSocket.PingInterval = 0 },
			wantErr: "websocket.ping_interval must be positive",
		},
		{
			name:    "outbound_buffer zero",
			modify:  func(c *Config) { c.WebSocket.OutboundBuffer = 0 },
			wantErr: "websocket.outbound_buffer must be positive",
		},
		{
			name:    "max_message_size zero",
			modify:  func(c *Config) { c.WebSocket.MaxMessageSize = 0 },
			wantErr: "websocket.max_message_size must be positive",
		},
		{
			name:    "default_ttl zero",
			modify:  func(c *Config) { c.Retention.DefaultTTL = 0 },
			wantErr: "retention.default_ttl must be positive",
		},
		{
			name:    "default_max_count zero",
			modify:  func(c *Config) { c.Retention.DefaultMaxCount = 0 },
			wantErr: "retention.default_max_count must be positive",
		},
		{
			name:    "eviction_interval zero",
			modify:  func(c *Config) { c.Retention.EvictionInterval = 0 },
			wantErr: "retention.eviction_interval must be positive",
		},
		{
			name:    "shutdown timeout zero",
			modify:  func(c *Config) { c.Shutdown.Timeout = 0 },
			wantErr: "shutdown.timeout must be positive",
		},
		{
			name:    "invalid log level",
			modify:  func(c *Config) { c.Log.Level = "verbose" },
			wantErr: "log.level must be one of",
		},
		{
			name:    "invalid log format",
			modify:  func(c *Config) { c.Log.Format = "xml" },
			wantErr: "log.format must be one of",
		},
		{
			name:    "pong_timeout less than ping_interval",
			modify:  func(c *Config) { c.WebSocket.PongTimeout = 10 * time.Second },
			wantErr: "websocket.pong_timeout must be >= websocket.ping_interval",
		},
		{
			name:    "tls_cert without tls_key",
			modify:  func(c *Config) { c.Server.TLSCert = "/path/cert.pem" },
			wantErr: "server.tls_key is required",
		},
		{
			name:    "tls_key without tls_cert",
			modify:  func(c *Config) { c.Server.TLSKey = "/path/key.pem" },
			wantErr: "server.tls_cert is required",
		},
		{
			name:    "api_key too short",
			modify:  func(c *Config) { c.Auth.APIKeys = []APIKeyEntry{{Key: "short", Description: "test"}} },
			wantErr: "auth.api_keys[0].key must be at least 43 characters",
		},
		{
			name:    "api_key invalid chars",
			modify:  func(c *Config) { c.Auth.APIKeys = []APIKeyEntry{{Key: "has spaces and special!@#chars here padding!!!", Description: "test"}} },
			wantErr: "auth.api_keys[0].key contains invalid characters",
		},
		{
			name:    "allowed_origins invalid format",
			modify:  func(c *Config) { c.WebSocket.AllowedOrigins = []string{"ftp://bad.example.com"} },
			wantErr: "websocket.allowed_origins",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Database.DSN = "postgres://localhost/aether"
			cfg.Auth.JWTSigningKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			tt.modify(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestValidate_AllowedOrigins(t *testing.T) {
	cfg := defaultConfig()
	cfg.Database.DSN = "postgres://localhost/aether"
	cfg.Auth.JWTSigningKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// "*" is valid
	cfg.WebSocket.AllowedOrigins = []string{"*"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected '*' to be valid, got %v", err)
	}

	// "https://example.com" is valid
	cfg.WebSocket.AllowedOrigins = []string{"https://example.com"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected https origin to be valid, got %v", err)
	}

	// "http://localhost:3000" is valid
	cfg.WebSocket.AllowedOrigins = []string{"http://localhost:3000"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected http origin to be valid, got %v", err)
	}

	// invalid scheme
	cfg.WebSocket.AllowedOrigins = []string{"ftp://bad.com"}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for invalid origin scheme")
	}
}

func TestValidate_RetentionRules(t *testing.T) {
	cfg := defaultConfig()
	cfg.Database.DSN = "postgres://localhost/aether"
	cfg.Auth.JWTSigningKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	cfg.Retention.Rules = []RetentionRule{
		{Pattern: "", TTL: 24 * time.Hour, MaxCount: 100},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for empty pattern")
	}

	cfg.Retention.Rules = []RetentionRule{
		{Pattern: "alerts.*", TTL: 0, MaxCount: 100},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for zero TTL")
	}

	cfg.Retention.Rules = []RetentionRule{
		{Pattern: "alerts.*", TTL: 24 * time.Hour, MaxCount: 0},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for zero max_count")
	}

	// invalid pattern: bare "*"
	cfg.Retention.Rules = []RetentionRule{
		{Pattern: "*", TTL: 24 * time.Hour, MaxCount: 100},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for bare * pattern")
	}

	// invalid pattern: not a valid channel name
	cfg.Retention.Rules = []RetentionRule{
		{Pattern: "has space.*", TTL: 24 * time.Hour, MaxCount: 100},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid prefix in pattern")
	}

	// valid pattern
	cfg.Retention.Rules = []RetentionRule{
		{Pattern: "alerts.*", TTL: 24 * time.Hour, MaxCount: 5000},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid rule, got %v", err)
	}
}

func TestChannelNameRegex(t *testing.T) {
	valid := []string{
		"a",
		"order.1234",
		"system.alerts",
		"iot.temp-sensor_01",
		"a/b-c_d",
		"alerts",
		strings.Repeat("a", 128),
	}
	for _, name := range valid {
		if !ChannelNameRegex.MatchString(name) {
			t.Errorf("expected %q to be valid", name)
		}
	}

	invalid := []string{
		"",
		".leading",
		"trailing.",
		"double..dot",
		"has space",
		"has*star",
		strings.Repeat("a", 129),
		"has中文",
	}
	for _, name := range invalid {
		if ChannelNameRegex.MatchString(name) {
			t.Errorf("expected %q to be invalid", name)
		}
	}
}

func TestMatchRetentionRule(t *testing.T) {
	cfg := &Config{
		Retention: RetentionConfig{
			DefaultTTL:       720 * time.Hour,
			DefaultMaxCount:  10000,
			Rules: []RetentionRule{
				{Pattern: "alerts.*", TTL: 24 * time.Hour, MaxCount: 5000},
				{Pattern: "orders.*", TTL: 2160 * time.Hour, MaxCount: 50000},
				{Pattern: "iot.*", TTL: 168 * time.Hour, MaxCount: 100000},
			},
		},
	}

	tests := []struct {
		channel string
		wantTTL time.Duration
		wantMax int
	}{
		{"alerts.critical", 24 * time.Hour, 5000},
		{"alerts.nested.deep", 24 * time.Hour, 5000},
		{"orders.1234", 2160 * time.Hour, 50000},
		{"iot.temp", 168 * time.Hour, 100000},
		{"iot.nested.sensor", 168 * time.Hour, 100000},
		{"unknown.channel", 720 * time.Hour, 10000},
		{"alerting", 720 * time.Hour, 10000},
	}

	for _, tt := range tests {
		ttl, maxCount := cfg.MatchRetentionRule(tt.channel)
		if ttl != tt.wantTTL || maxCount != tt.wantMax {
			t.Errorf("MatchRetentionRule(%q) = (%v, %d), want (%v, %d)", tt.channel, ttl, maxCount, tt.wantTTL, tt.wantMax)
		}
	}
}

func TestLoad_FullConfig(t *testing.T) {
	path := writeTestConfig(t, `
server:
  addr: ":9090"
  tls_cert: "/cert.pem"
  tls_key: "/key.pem"
database:
  dsn: "postgres://user:pass@db:5432/aether"
  max_open_conns: 50
  max_idle_conns: 20
  conn_max_idle_time: 10m
  conn_max_lifetime: 1h
auth:
  jwt_signing_key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  jwt_clock_skew: 1m
  api_keys:
    - key: "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY"
      description: "test"
websocket:
  ping_interval: 15s
  pong_timeout: 30s
  outbound_buffer: 512
  max_message_size: 131072
  allowed_origins:
    - "https://app.example.com"
retention:
  default_ttl: 48h
  default_max_count: 5000
  eviction_interval: 10m
  rules:
    - pattern: "alerts.*"
      ttl: 12h
      max_count: 2000
shutdown:
  timeout: 30s
log:
  level: "debug"
  format: "text"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Addr != ":9090" {
		t.Errorf("Server.Addr = %q", cfg.Server.Addr)
	}
	if cfg.Database.MaxOpenConns != 50 {
		t.Errorf("Database.MaxOpenConns = %d", cfg.Database.MaxOpenConns)
	}
	if cfg.Auth.JWTSigningKey != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("Auth.JWTSigningKey not loaded")
	}
	if len(cfg.Auth.APIKeys) != 1 || cfg.Auth.APIKeys[0].Key != "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY" {
		t.Errorf("Auth.APIKeys not loaded correctly")
	}
	if cfg.WebSocket.PingInterval != 15*time.Second {
		t.Errorf("WebSocket.PingInterval = %v", cfg.WebSocket.PingInterval)
	}
	if len(cfg.WebSocket.AllowedOrigins) != 1 {
		t.Errorf("WebSocket.AllowedOrigins len = %d", len(cfg.WebSocket.AllowedOrigins))
	}
	if len(cfg.Retention.Rules) != 1 {
		t.Errorf("Retention.Rules len = %d", len(cfg.Retention.Rules))
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q", cfg.Log.Level)
	}
	if cfg.Log.Format != "text" {
		t.Errorf("Log.Format = %q", cfg.Log.Format)
	}
}

// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi
//
// JSON config + flag parsing.
//
// Spec (agreed):
// - Positional args are NOT supported (error if present).
// - Config file is loaded ONLY when -config is explicitly provided (no implicit ./config.json).
// - Flags override JSON config.
// - URI hiding/refactor is postponed (keep register_uri/message_uri/entrance_uri as-is).
// - Split "enabled" into 2 toggles:
//     * answer_calls: whether to Answer INVITE (otherwise reject)
//     * send_messages: whether to send MESSAGE (initial/entrance)
//
// Notes:
// - Allowed callers can be specified multiple times: --allowed-caller alice --allowed-caller bob
// - Error messages should list missing required fields.

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const passwordEnvVar = "AP_INTERCOM_PASSWORD"

// Duration is a JSON-friendly duration.
// It accepts either a JSON string (e.g. "3600s") or a number (seconds).
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	// Try string first.
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		dd, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		d.Duration = dd
		return nil
	}

	// Fallback: number => seconds.
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	d.Duration = time.Duration(n * float64(time.Second))
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Duration.String())
}

type Config struct {
	// Auth / registrar
	Username  string `json:"username"`
	Password  string `json:"password"`
	ProxyHost string `json:"proxy_host"`

	// SIP UA settings
	UserAgentHostname string `json:"user_agent_hostname"`
	LogLevel          string `json:"log_level"`

	// Local bind for SIP server (diago transport)
	BindHost string `json:"bind_host"`
	BindPort int    `json:"bind_port"`

	// Contact header MUST be correct in your environment.
	// Example produced header:
	//   <sip:USER@192.168.100.5:15060>;+l.operator.docomo.intercom
	ContactHost   string `json:"contact_host"`
	ContactPort   int    `json:"contact_port"`
	ContactParams string `json:"contact_params"`

	// Registration behavior
	Expiry        Duration `json:"expiry"`
	RetryInterval Duration `json:"retry_interval"`

	// Target URIs (kept as-is; URI hiding is postponed)
	RegisterURI string `json:"register_uri"`
	MessageURI  string `json:"message_uri"`
	EntranceURI string `json:"entrance_uri"`

	// One-shot monitor probe. When MonitorJPEGOut is set, it polls the JPEG
	// endpoint advertised by the SIP answer while the dialog is confirmed.
	MonitorProbe        bool     `json:"monitor_probe,omitempty"`
	MonitorURI          string   `json:"monitor_uri,omitempty"`
	MonitorHold         Duration `json:"monitor_hold,omitempty"`
	MonitorRepeat       int      `json:"monitor_repeat,omitempty"`
	MonitorJPEGOut      string   `json:"monitor_jpeg_out,omitempty"`
	MonitorJPEGInterval Duration `json:"monitor_jpeg_interval,omitempty"`
	JPEGQueryS          string   `json:"jpeg_query_s"`

	// Incoming ring JPEG capture. When automatic unlock is enabled at ring
	// time, a non-empty directory enables a reliable early dialog (183/PRACK)
	// and saves one timestamped JPEG per call.
	IncomingJPEGDir      string   `json:"incoming_jpeg_dir,omitempty"`
	IncomingJPEGHold     Duration `json:"incoming_jpeg_hold,omitempty"`
	IncomingJPEGInterval Duration `json:"incoming_jpeg_interval,omitempty"`
	IncomingJPEGMaxFiles int      `json:"incoming_jpeg_max_files,omitempty"`

	// MESSAGE payloads
	MessageContentType  string `json:"message_content_type"`
	MessageBody         string `json:"message_body"`
	EntranceContentType string `json:"entrance_content_type"`
	EntranceBody        string `json:"entrance_body"`

	// Behavior toggles
	AnswerCalls  bool `json:"answer_calls"`
	SendMessages bool `json:"send_messages"`

	// Reject behavior when not answering
	RejectStatus int    `json:"reject_status"` // 480, 486, or 503
	RejectReason string `json:"reject_reason"` // e.g. "Temporarily Unavailable" or "Service Unavailable"

	// HTTP control plane (optional)
	HTTPListen string `json:"http_listen"` // e.g. "127.0.0.1:18080"

	// Inbound MESSAGE buffer
	InboundMessageBuffer int `json:"inbound_message_buffer"`

	// Allowed callers filter (user part only, in your assumption)
	AllowedCallers []string `json:"allowed_callers"`

	// Callers that may receive the automatic-unlock MESSAGE. This is a
	// separate allow-list because some allowed callers are camera-only doors.
	UnlockCallers []string `json:"unlock_callers"`

	// LoadedConfigPath is internal metadata (not part of config.json).
	LoadedConfigPath string `json:"-"`
}

func DefaultConfig() Config {
	return Config{
		ProxyHost:            "192.168.100.25:5060",
		UserAgentHostname:    "192.168.100.25",
		LogLevel:             "",
		BindHost:             "0.0.0.0",
		BindPort:             15060,
		ContactHost:          "",
		ContactPort:          15060,
		ContactParams:        ";+l.operator.docomo.intercom",
		Expiry:               Duration{Duration: 600 * time.Second},
		RetryInterval:        Duration{Duration: 30 * time.Second},
		AnswerCalls:          true,
		SendMessages:         true,
		RejectStatus:         503,
		RejectReason:         "Service Unavailable",
		HTTPListen:           "127.0.0.1:18080",
		InboundMessageBuffer: 100,
		MessageContentType:   "application/intercom.message+xml",
		MessageBody:          "<?xml version=\"1.0\" encoding=\"UTF-8\" ?><IFBOX version = \"1.0\"><SYSTEM><SYS_COMMAND><USER_COMM  state = \"request\"  type = \"housing\">ALL</USER_COMM></SYS_COMMAND></SYSTEM></IFBOX>",
		EntranceContentType:  "application/intercom.message+xml",
		EntranceBody:         "<?xml version=\"1.0\" encoding=\"UTF-8\" ?>\r\n<IFBOX version = \"1.0\">\r\n    <SYSTEM>\r\n        <SYS_COMMAND>\r\n            <USER_COMM  state = \"request\"  type = \"lock\">UNLOCK</USER_COMM>\r\n        </SYS_COMMAND>\r\n    </SYSTEM>\r\n</IFBOX>\r\n",
		MonitorHold:          Duration{Duration: 3 * time.Second},
		MonitorRepeat:        2,
		MonitorJPEGInterval:  Duration{Duration: time.Second},
		JPEGQueryS:           defaultJPEGQueryS,
		IncomingJPEGHold:     Duration{Duration: 5 * time.Second},
		IncomingJPEGInterval: Duration{Duration: time.Second},
		IncomingJPEGMaxFiles: 100,
		UnlockCallers:        []string{"interphone0"},
	}
}

func LoadConfigFile(path string) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func SaveConfigJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func missingRequiredError(missing []string) error {
	return fmt.Errorf("missing required: %s", strings.Join(missing, ", "))
}

// ParseFlags loads JSON config only when -config is explicitly provided,
// then applies flag overrides. Positional args are NOT supported.
func ParseFlags(args []string) (Config, error) {
	// First pass: find -config in argv without full flag parsing.
	configPath := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "-config" && i+1 < len(args) {
			configPath = args[i+1]
			break
		}
		if strings.HasPrefix(args[i], "-config=") {
			configPath = strings.TrimPrefix(args[i], "-config=")
			break
		}
	}

	// Only load a config file when explicitly requested.
	cfg := DefaultConfig()
	if configPath != "" {
		loaded, err := LoadConfigFile(configPath)
		if err != nil {
			return cfg, fmt.Errorf("load config: %w", err)
		}
		cfg = loaded
		cfg.LoadedConfigPath = configPath
	}
	if cfg.Password == "" {
		cfg.Password = os.Getenv(passwordEnvVar)
	}

	fs := flag.NewFlagSet("entrance-go", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	// Flags override config.
	fs.StringVar(&configPath, "config", configPath, "Path to JSON config file (loaded only when explicitly provided; flags override it)")

	fs.StringVar(&cfg.Username, "username", cfg.Username, "Digest username (required)")
	fs.StringVar(&cfg.Password, "password", cfg.Password, "Digest password (required)")
	fs.StringVar(&cfg.ProxyHost, "proxy", cfg.ProxyHost, "Outbound proxy host:port (registrar) (required)")
	fs.StringVar(&cfg.UserAgentHostname, "uahost", cfg.UserAgentHostname, "SIP User-Agent hostname")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log level (debug, info, warn, error)")

	fs.StringVar(&cfg.BindHost, "bind-host", cfg.BindHost, "Local bind host for SIP server transport")
	fs.IntVar(&cfg.BindPort, "bind-port", cfg.BindPort, "Local bind port for SIP server transport")

	fs.StringVar(&cfg.ContactHost, "contact-host", cfg.ContactHost, "Host part for Contact header")
	fs.IntVar(&cfg.ContactPort, "contact-port", cfg.ContactPort, "Port part for Contact header")
	fs.StringVar(&cfg.ContactParams, "contact-params", cfg.ContactParams, "Additional Contact params (e.g. ;+l.operator.docomo.intercom)")

	fs.DurationVar(&cfg.Expiry.Duration, "expiry", cfg.Expiry.Duration, "REGISTER expiry (e.g. 3600s)")
	fs.DurationVar(&cfg.RetryInterval.Duration, "retry", cfg.RetryInterval.Duration, "REGISTER retry interval (e.g. 60s)")

	fs.BoolVar(&cfg.AnswerCalls, "answer-calls", cfg.AnswerCalls, "Answer incoming INVITE calls")
	fs.BoolVar(&cfg.SendMessages, "send-messages", cfg.SendMessages, "Send SIP MESSAGE (initial/entrance)")

	fs.IntVar(&cfg.RejectStatus, "reject-code", cfg.RejectStatus, "Reject status code when not answering (480, 486, or 503)")
	fs.StringVar(&cfg.RejectReason, "reject-reason", cfg.RejectReason, "Reject reason phrase when not answering")

	fs.StringVar(&cfg.HTTPListen, "http", cfg.HTTPListen, "HTTP listen address (empty disables HTTP)")
	fs.IntVar(&cfg.InboundMessageBuffer, "msgbuf", cfg.InboundMessageBuffer, "Inbound MESSAGE buffer size")

	// Keep URI flags for now (URI hiding postponed)
	fs.StringVar(&cfg.RegisterURI, "register-uri", cfg.RegisterURI, "REGISTER target SIP URI (required)")
	fs.StringVar(&cfg.MessageURI, "message-uri", cfg.MessageURI, "MESSAGE target SIP URI (required)")
	fs.StringVar(&cfg.EntranceURI, "entrance-uri", cfg.EntranceURI, "Entrance target SIP URI (required)")
	fs.BoolVar(&cfg.MonitorProbe, "monitor-probe", cfg.MonitorProbe, "Run the one-shot SIP monitor probe and exit")
	fs.StringVar(&cfg.MonitorURI, "monitor-uri", cfg.MonitorURI, "Monitor target SIP URI (required with --monitor-probe)")
	fs.DurationVar(&cfg.MonitorHold.Duration, "monitor-hold", cfg.MonitorHold.Duration, "How long each probe dialog remains established")
	fs.IntVar(&cfg.MonitorRepeat, "monitor-repeat", cfg.MonitorRepeat, "Number of sequential monitor dialogs in probe mode")
	fs.StringVar(&cfg.MonitorJPEGOut, "monitor-jpeg-out", cfg.MonitorJPEGOut, "Absolute path atomically updated with the latest valid monitor JPEG")
	fs.DurationVar(&cfg.MonitorJPEGInterval.Duration, "monitor-jpeg-interval", cfg.MonitorJPEGInterval.Duration, "Minimum interval between monitor JPEG requests")
	fs.StringVar(&cfg.JPEGQueryS, "jpeg-query-s", cfg.JPEGQueryS, "Value of the IFBOX JPEG HTTP query parameter s")
	fs.StringVar(&cfg.IncomingJPEGDir, "incoming-jpeg-dir", cfg.IncomingJPEGDir, "Absolute directory for timestamped JPEGs captured during automatic unlock")
	fs.DurationVar(&cfg.IncomingJPEGHold.Duration, "incoming-jpeg-hold", cfg.IncomingJPEGHold.Duration, "How long to poll JPEGs for each incoming ring")
	fs.DurationVar(&cfg.IncomingJPEGInterval.Duration, "incoming-jpeg-interval", cfg.IncomingJPEGInterval.Duration, "Minimum interval between incoming-ring JPEG requests")
	fs.IntVar(&cfg.IncomingJPEGMaxFiles, "incoming-jpeg-max-files", cfg.IncomingJPEGMaxFiles, "Maximum number of timestamped incoming-ring JPEGs retained")

	fs.StringVar(&cfg.MessageContentType, "msg-ct", cfg.MessageContentType, "Content-Type for MESSAGE to message_uri")
	fs.StringVar(&cfg.MessageBody, "msg-body", cfg.MessageBody, "Body for MESSAGE to message_uri")
	fs.StringVar(&cfg.EntranceContentType, "entrance-ct", cfg.EntranceContentType, "Content-Type for MESSAGE to entrance_uri")
	fs.StringVar(&cfg.EntranceBody, "entrance-body", cfg.EntranceBody, "Body for MESSAGE to entrance_uri")

	// Allowed callers: empty means allow-all (your policy).
	// Repeated flags append to the values already supplied by defaults or JSON.
	fs.Var((*StringList)(&cfg.AllowedCallers), "allowed-caller", "Allowed caller users (can be specified multiple times)")
	fs.Var(&ReplacingStringList{values: &cfg.UnlockCallers}, "unlock-caller", "Caller users eligible for automatic unlock; explicit flags replace the configured default")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"Usage:\n  %s [flags]\n\nRequired (unless set via -config JSON):\n"+
				"  --username --password (or "+passwordEnvVar+") --proxy --register-uri --message-uri --entrance-uri\n\nFlags:\n",
			os.Args[0],
		)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}

	// Positional args are not supported.
	if len(fs.Args()) > 0 {
		return cfg, fmt.Errorf("positional args are not supported; use flags only (try -h)")
	}

	// Validation: list missing required fields.
	var missing []string
	if cfg.Username == "" {
		missing = append(missing, "username (--username)")
	}
	if cfg.Password == "" {
		missing = append(missing, "password (--password or "+passwordEnvVar+")")
	}
	if cfg.ProxyHost == "" {
		missing = append(missing, "proxy_host (--proxy)")
	}
	if cfg.RegisterURI == "" {
		missing = append(missing, "register_uri (--register-uri)")
	}
	if cfg.MessageURI == "" {
		missing = append(missing, "message_uri (--message-uri)")
	}
	if cfg.EntranceURI == "" {
		missing = append(missing, "entrance_uri (--entrance-uri)")
	}
	if len(missing) > 0 {
		return cfg, missingRequiredError(missing)
	}

	if cfg.RejectStatus != 480 && cfg.RejectStatus != 486 && cfg.RejectStatus != 503 {
		return cfg, fmt.Errorf("reject-code must be 480, 486, or 503")
	}
	if cfg.MonitorProbe && cfg.MonitorURI == "" {
		return cfg, fmt.Errorf("monitor-uri is required with monitor-probe")
	}
	if cfg.MonitorProbe {
		if cfg.MonitorHold.Duration < 0 {
			return cfg, fmt.Errorf("monitor-hold must not be negative")
		}
		if cfg.MonitorRepeat < 1 {
			return cfg, fmt.Errorf("monitor-repeat must be at least 1")
		}
	}
	if cfg.MonitorJPEGOut != "" {
		if !cfg.MonitorProbe {
			return cfg, fmt.Errorf("monitor-jpeg-out requires monitor-probe")
		}
		if !filepath.IsAbs(cfg.MonitorJPEGOut) {
			return cfg, fmt.Errorf("monitor-jpeg-out must be an absolute path")
		}
		if cfg.MonitorHold.Duration <= 0 {
			return cfg, fmt.Errorf("monitor-hold must be positive with monitor-jpeg-out")
		}
		if cfg.MonitorJPEGInterval.Duration <= 0 {
			return cfg, fmt.Errorf("monitor-jpeg-interval must be positive")
		}
	}
	if strings.TrimSpace(cfg.JPEGQueryS) == "" {
		return cfg, fmt.Errorf("jpeg-query-s must not be empty")
	}
	if cfg.IncomingJPEGDir != "" {
		if !filepath.IsAbs(cfg.IncomingJPEGDir) {
			return cfg, fmt.Errorf("incoming-jpeg-dir must be an absolute path")
		}
		if cfg.IncomingJPEGHold.Duration <= 0 {
			return cfg, fmt.Errorf("incoming-jpeg-hold must be positive")
		}
		if cfg.IncomingJPEGInterval.Duration <= 0 {
			return cfg, fmt.Errorf("incoming-jpeg-interval must be positive")
		}
		if cfg.IncomingJPEGMaxFiles < 1 {
			return cfg, fmt.Errorf("incoming-jpeg-max-files must be at least 1")
		}
	}

	return cfg, nil
}

type StringList []string

func (s *StringList) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *StringList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	*s = append(*s, v)
	return nil
}

// ReplacingStringList makes repeated CLI values replace a JSON/default slice
// as a group. This matters for the security-sensitive unlock allow-list: an
// explicit flag must be able to remove the default caller.
type ReplacingStringList struct {
	values *[]string
	set    bool
}

func (s *ReplacingStringList) String() string {
	if s == nil || s.values == nil {
		return ""
	}
	return strings.Join(*s.values, ",")
}

func (s *ReplacingStringList) Set(value string) error {
	if s.values == nil {
		return fmt.Errorf("replacement string list has no target")
	}
	if !s.set {
		*s.values = nil
		s.set = true
	}
	value = strings.TrimSpace(value)
	if value != "" {
		*s.values = append(*s.values, value)
	}
	return nil
}

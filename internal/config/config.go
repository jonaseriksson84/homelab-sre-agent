// Package config loads all runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Environment variable names. Load reads exactly these; EnvVars exports the
// full list so packaging surfaces (the Unraid CA template test) can verify
// they haven't drifted from what the binary actually reads.
const (
	EnvAnthropicKey        = "ANTHROPIC_API_KEY"
	EnvLokiURL             = "SRE_LOKI_URL"
	EnvLokiContainerLabel  = "SRE_LOKI_CONTAINER_LABEL"
	EnvPrometheusURL       = "SRE_PROMETHEUS_URL"
	EnvDockerProxyURL      = "SRE_DOCKER_PROXY_URL"
	EnvNtfyURL             = "SRE_NTFY_URL"
	EnvNtfyTopic           = "SRE_NTFY_TOPIC"
	EnvAnthropicURL        = "SRE_ANTHROPIC_URL"
	EnvTriageModel         = "SRE_TRIAGE_MODEL"
	EnvEscalationModel     = "SRE_ESCALATION_MODEL"
	EnvConfidenceThreshold = "SRE_CONFIDENCE_THRESHOLD"
	EnvLogByteBudget       = "SRE_LOG_BYTE_BUDGET"
	EnvMemoryWindowDays    = "SRE_MEMORY_WINDOW_DAYS"
	EnvMemoryMaxEntries    = "SRE_MEMORY_MAX_ENTRIES"
	EnvToolBudget          = "SRE_TOOL_BUDGET"
	EnvListenAddr          = "SRE_LISTEN_ADDR"
	EnvMCPListenAddr       = "SRE_MCP_LISTEN_ADDR"
	EnvDBPath              = "SRE_DB_PATH"
)

// EnvVars is every environment variable Load reads.
var EnvVars = []string{
	EnvAnthropicKey,
	EnvLokiURL,
	EnvLokiContainerLabel,
	EnvPrometheusURL,
	EnvDockerProxyURL,
	EnvNtfyURL,
	EnvNtfyTopic,
	EnvAnthropicURL,
	EnvTriageModel,
	EnvEscalationModel,
	EnvConfidenceThreshold,
	EnvLogByteBudget,
	EnvMemoryWindowDays,
	EnvMemoryMaxEntries,
	EnvToolBudget,
	EnvListenAddr,
	EnvMCPListenAddr,
	EnvDBPath,
}

type Config struct {
	LokiURL             string
	LokiContainerLabel  string
	PrometheusURL       string
	DockerProxyURL      string
	NtfyURL             string
	NtfyTopic           string
	AnthropicURL        string
	AnthropicKey        string
	TriageModel         string
	EscalationModel     string
	ConfidenceThreshold float64
	LogByteBudget       int
	MemoryWindowDays    int
	MemoryMaxEntries    int
	ToolBudget          int
	ListenAddr          string
	MCPListenAddr       string
	DBPath              string
}

func Load() (Config, error) {
	c := Config{
		LokiURL:            getenv(EnvLokiURL, "http://loki:3100"),
		LokiContainerLabel: getenv(EnvLokiContainerLabel, "container_name"),
		PrometheusURL:      getenv(EnvPrometheusURL, "http://prometheus:9090"),
		DockerProxyURL:     getenv(EnvDockerProxyURL, "http://docker-proxy:2375"),
		NtfyURL:            getenv(EnvNtfyURL, "https://ntfy.sh"),
		NtfyTopic:          os.Getenv(EnvNtfyTopic),
		AnthropicURL:       getenv(EnvAnthropicURL, "https://api.anthropic.com"),
		AnthropicKey:       os.Getenv(EnvAnthropicKey),
		TriageModel:        getenv(EnvTriageModel, "claude-haiku-4-5"),
		EscalationModel:    getenv(EnvEscalationModel, "claude-opus-4-8"),
		ListenAddr:         getenv(EnvListenAddr, ":8080"),
		MCPListenAddr:      os.Getenv(EnvMCPListenAddr), // empty = MCP disabled
		DBPath:             getenv(EnvDBPath, "incidents.db"),
	}
	var err error
	if c.ConfidenceThreshold, err = getfloat(EnvConfidenceThreshold, 0.7); err != nil {
		return c, err
	}
	budget, err := getint(EnvLogByteBudget, 40960)
	if err != nil {
		return c, err
	}
	c.LogByteBudget = budget
	if c.MemoryWindowDays, err = getint(EnvMemoryWindowDays, 30); err != nil {
		return c, err
	}
	if c.MemoryMaxEntries, err = getint(EnvMemoryMaxEntries, 5); err != nil {
		return c, err
	}
	if c.ToolBudget, err = getint(EnvToolBudget, 5); err != nil {
		return c, err
	}
	if c.AnthropicKey == "" {
		return c, fmt.Errorf("%s is required", EnvAnthropicKey)
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getfloat(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
}

func getint(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

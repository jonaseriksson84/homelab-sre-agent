// Package config loads all runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
)

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
	ListenAddr          string
	DBPath              string
}

func Load() (Config, error) {
	c := Config{
		LokiURL:            getenv("SRE_LOKI_URL", "http://loki:3100"),
		LokiContainerLabel: getenv("SRE_LOKI_CONTAINER_LABEL", "container_name"),
		PrometheusURL:      getenv("SRE_PROMETHEUS_URL", "http://prometheus:9090"),
		DockerProxyURL:     getenv("SRE_DOCKER_PROXY_URL", "http://docker-proxy:2375"),
		NtfyURL:            getenv("SRE_NTFY_URL", "https://ntfy.sh"),
		NtfyTopic:          os.Getenv("SRE_NTFY_TOPIC"),
		AnthropicURL:       getenv("SRE_ANTHROPIC_URL", "https://api.anthropic.com"),
		AnthropicKey:       os.Getenv("ANTHROPIC_API_KEY"),
		TriageModel:        getenv("SRE_TRIAGE_MODEL", "claude-haiku-4-5"),
		EscalationModel:    getenv("SRE_ESCALATION_MODEL", "claude-opus-4-8"),
		ListenAddr:         getenv("SRE_LISTEN_ADDR", ":8080"),
		DBPath:             getenv("SRE_DB_PATH", "incidents.db"),
	}
	var err error
	if c.ConfidenceThreshold, err = getfloat("SRE_CONFIDENCE_THRESHOLD", 0.7); err != nil {
		return c, err
	}
	budget, err := getint("SRE_LOG_BYTE_BUDGET", 40960)
	if err != nil {
		return c, err
	}
	c.LogByteBudget = budget
	if c.MemoryWindowDays, err = getint("SRE_MEMORY_WINDOW_DAYS", 30); err != nil {
		return c, err
	}
	if c.MemoryMaxEntries, err = getint("SRE_MEMORY_MAX_ENTRIES", 5); err != nil {
		return c, err
	}
	if c.AnthropicKey == "" {
		return c, fmt.Errorf("ANTHROPIC_API_KEY is required")
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

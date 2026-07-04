// Package notify pushes Diagnoses to the operator. ntfy is the only
// implementation; the Notifier interface leaves the door open for others.
package notify

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Notifier interface {
	// Notify sends a message. Priority follows ntfy semantics: 5=max … 1=min.
	Notify(ctx context.Context, title, message string, priority int) error
}

// SeverityPriority maps a Diagnosis severity to an ntfy priority.
func SeverityPriority(severity string) int {
	switch severity {
	case "critical":
		return 5
	case "warning":
		return 3
	default:
		return 2
	}
}

const ResolvedPriority = 2

type Ntfy struct {
	baseURL string
	topic   string
	client  *http.Client
}

func NewNtfy(baseURL, topic string) *Ntfy {
	return &Ntfy{baseURL: baseURL, topic: topic, client: &http.Client{Timeout: 30 * time.Second}}
}

func (n *Ntfy) Notify(ctx context.Context, title, message string, priority int) error {
	url := fmt.Sprintf("%s/%s", strings.TrimSuffix(n.baseURL, "/"), n.topic)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(message))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	req.Header.Set("Priority", fmt.Sprintf("%d", priority))
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy status %d", resp.StatusCode)
	}
	return nil
}

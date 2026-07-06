package main

import (
	"encoding/xml"
	"os"
	"slices"
	"testing"

	"github.com/jonaseriksson84/homelab-sre-agent/internal/config"
)

// Env vars deliberately absent from the Unraid template, with the reason.
var templateExclusions = map[string]string{
	config.EnvListenAddr:   "fixed at :8080 inside the container; Unraid's port mapping is the knob",
	config.EnvAnthropicURL: "test seam for pointing the Claude client at fakes; not operator-facing",
}

type caTemplate struct {
	Configs []struct {
		Name   string `xml:"Name,attr"`
		Target string `xml:"Target,attr"`
		Type   string `xml:"Type,attr"`
		Mask   string `xml:"Mask,attr"`
	} `xml:"Config"`
}

// The CA template and the config package must not drift: every Variable in
// the template is one the binary reads, and every env var the binary reads is
// in the template unless excluded above.
func TestUnraidTemplateMatchesConfigEnvVars(t *testing.T) {
	raw, err := os.ReadFile("unraid/sre-agent.xml")
	if err != nil {
		t.Fatal(err)
	}
	var tpl caTemplate
	if err := xml.Unmarshal(raw, &tpl); err != nil {
		t.Fatalf("parsing template: %v", err)
	}

	var templateVars []string
	for _, c := range tpl.Configs {
		if c.Type != "Variable" {
			continue
		}
		templateVars = append(templateVars, c.Target)
		if c.Target == config.EnvAnthropicKey && c.Mask != "true" {
			t.Errorf("template field %q (%s) must be password-masked", c.Name, c.Target)
		}
	}

	for _, v := range templateVars {
		if !slices.Contains(config.EnvVars, v) {
			t.Errorf("template declares %s, but the binary does not read it", v)
		}
	}
	for _, v := range config.EnvVars {
		if _, excluded := templateExclusions[v]; excluded {
			if slices.Contains(templateVars, v) {
				t.Errorf("%s is both in the template and in templateExclusions — drop one", v)
			}
			continue
		}
		if !slices.Contains(templateVars, v) {
			t.Errorf("binary reads %s, but the template does not expose it (add it or record an exclusion)", v)
		}
	}
}

package agent

import (
	"strings"
	"testing"
)

func TestPromptExplainsImageInstall(t *testing.T) {
	prompt := strings.Join(strings.Fields(systemPrompt), " ") // the prompt wraps its lines
	for _, want := range []string{`"install"`, "Debian bookworm", "pin versions", `["apt-get","update"]`, "at build time", "no credentials"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
}

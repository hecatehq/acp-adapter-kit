//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package commandbridge

import (
	"strings"
	"testing"
)

func TestOpenPromptResourceFailsClosedOnUnsupportedPlatform(t *testing.T) {
	file, err := openPromptResource("ignored")
	if file != nil || err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("openPromptResource = %#v, %v; want actionable unsupported-platform error", file, err)
	}
	if err := securePromptResourceStage("ignored"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("securePromptResourceStage error = %v, want actionable unsupported-platform error", err)
	}
}

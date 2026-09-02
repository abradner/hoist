package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// opRead runs `op read <ref>` and returns the secret. It is a variable so tests can
// substitute it — and assert it is never called when no OpRef is configured. The error
// carries op's exit status only: its stderr can echo the reference and is not repeated.
var opRead = func(ctx context.Context, ref string) (string, error) {
	cmd := exec.CommandContext(ctx, "op", "read", "--no-newline", ref)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("op read failed (exit %d)", exit.ExitCode())
		}
		return "", fmt.Errorf("op read failed: %w", err) // op not installed, or the context ended
	}
	tok := strings.TrimSpace(out.String())
	if tok == "" {
		return "", errors.New("op read returned nothing")
	}
	return tok, nil
}

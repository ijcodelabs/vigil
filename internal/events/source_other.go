//go:build !linux

package events

import (
	"fmt"
	"log/slog"

	"github.com/ijcodelabs/vigil/internal/config"
)

func openSource(*config.Security, *slog.Logger) (source, error) {
	return nil, fmt.Errorf("%w: runtime events need Linux", ErrUnsupported)
}

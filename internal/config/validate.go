package config

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidPort is returned when a port is out of the valid TCP range
// (1..65535).
var ErrInvalidPort = errors.New("port must be between 1 and 65535")

// ValidatePort returns nil if port is within the valid TCP range
// (1 <= port <= 65535), otherwise it returns ErrInvalidPort.
func ValidatePort(port int) error {
	if port < 1 || port > 65535 {
		return ErrInvalidPort
	}
	return nil
}

// ValidateDomains validates a list of Domain entries:
//   - each Domain must be non-empty after trimming whitespace;
//   - domains must be unique case-insensitively (after trimming).
//
// The first problem encountered is returned.
func ValidateDomains(domains []Domain) error {
	seen := make(map[string]int, len(domains)) // lower-cased domain -> first index
	for i, d := range domains {
		trimmed := strings.TrimSpace(d.Domain)
		if trimmed == "" {
			return fmt.Errorf("domains[%d]: domain must not be empty", i)
		}
		key := strings.ToLower(trimmed)
		if prev, ok := seen[key]; ok {
			return fmt.Errorf("domains[%d]: duplicate domain %q (also at index %d)", i, trimmed, prev)
		}
		seen[key] = i
	}
	return nil
}

// ValidateForwards validates a list of Forward entries:
//   - ListenPort must be in 1..65535 and unique across the list;
//   - TargetHost must be non-empty after trimming whitespace;
//   - TargetPort must be in 1..65535.
//
// The first problem encountered is returned. ListenPort uniqueness is
// case-insensitively irrelevant (ports are ints); the check is on integer
// value.
func ValidateForwards(forwards []Forward) error {
	listenPorts := make(map[int]int, len(forwards)) // port -> first index
	for i, f := range forwards {
		if err := ValidatePort(f.ListenPort); err != nil {
			return fmt.Errorf("forwards[%d]: listen_port %w", i, err)
		}
		if prev, ok := listenPorts[f.ListenPort]; ok {
			return fmt.Errorf("forwards[%d]: duplicate listen_port %d (also at index %d)", i, f.ListenPort, prev)
		}
		listenPorts[f.ListenPort] = i

		if strings.TrimSpace(f.TargetHost) == "" {
			return fmt.Errorf("forwards[%d]: target_host must not be empty", i)
		}
		if err := ValidatePort(f.TargetPort); err != nil {
			return fmt.Errorf("forwards[%d]: target_port %w", i, err)
		}
	}
	return nil
}
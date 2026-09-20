package config

import (
	"errors"
	"testing"
)

func TestValidatePortBoundaries(t *testing.T) {
	cases := []struct {
		port  int
		valid bool
	}{
		{-1, false},
		{0, false},
		{1, true},
		{1024, true},
		{65535, true},
		{65536, false},
		{100000, false},
	}

	for _, tc := range cases {
		err := ValidatePort(tc.port)
		if tc.valid && err != nil {
			t.Errorf("ValidatePort(%d): expected nil, got %v", tc.port, err)
		}
		if !tc.valid && err == nil {
			t.Errorf("ValidatePort(%d): expected error, got nil", tc.port)
		}
		if !tc.valid && !errors.Is(err, ErrInvalidPort) {
			t.Errorf("ValidatePort(%d): expected ErrInvalidPort, got %v", tc.port, err)
		}
	}
}

func TestNewID(t *testing.T) {
	id := NewID()
	if len(id) != 8 {
		t.Fatalf("expected 8-char id, got %q (len %d)", id, len(id))
	}
	// Should be hex.
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("expected hex id, got %q", id)
		}
	}
	// Two calls should almost certainly differ.
	id2 := NewID()
	if id == id2 {
		t.Fatalf("expected distinct ids, got %q twice", id)
	}
}

func TestValidateDomainsEmpty(t *testing.T) {
	domains := []Domain{
		{ID: "a", Domain: ""},
	}
	if err := ValidateDomains(domains); err == nil {
		t.Fatal("expected error for empty domain, got nil")
	}

	domains[0].Domain = "   "
	if err := ValidateDomains(domains); err == nil {
		t.Fatal("expected error for whitespace-only domain, got nil")
	}
}

func TestValidateDomainsDuplicateCaseInsensitive(t *testing.T) {
	domains := []Domain{
		{ID: "a", Domain: "Tunnel.Example.com"},
		{ID: "b", Domain: "tunnel.example.com"},
	}
	if err := ValidateDomains(domains); err == nil {
		t.Fatal("expected error for case-insensitive duplicate domain, got nil")
	}
}

func TestValidateDomainsUniqueAfterTrim(t *testing.T) {
	domains := []Domain{
		{ID: "a", Domain: "  tunnel.example.com  "},
		{ID: "b", Domain: "other.example.com"},
	}
	if err := ValidateDomains(domains); err != nil {
		t.Fatalf("expected nil for unique domains, got %v", err)
	}
}

func TestValidateDomainsEmptyList(t *testing.T) {
	if err := ValidateDomains(nil); err != nil {
		t.Fatalf("expected nil for nil list, got %v", err)
	}
	if err := ValidateDomains([]Domain{}); err != nil {
		t.Fatalf("expected nil for empty list, got %v", err)
	}
}

func TestValidateForwardsListenPortBounds(t *testing.T) {
	cases := []int{0, -1, 65536}
	for _, p := range cases {
		fwd := []Forward{
			{ID: "a", ListenPort: p, TargetHost: "h.svc", TargetPort: 80},
		}
		err := ValidateForwards(fwd)
		if err == nil {
			t.Fatalf("expected error for listen_port=%d, got nil", p)
		}
		if !errors.Is(err, ErrInvalidPort) {
			t.Fatalf("expected ErrInvalidPort for listen_port=%d, got %v", p, err)
		}
	}
}

func TestValidateForwardsTargetPortBounds(t *testing.T) {
	cases := []int{0, -1, 65536}
	for _, p := range cases {
		fwd := []Forward{
			{ID: "a", ListenPort: 8080, TargetHost: "h.svc", TargetPort: p},
		}
		err := ValidateForwards(fwd)
		if err == nil {
			t.Fatalf("expected error for target_port=%d, got nil", p)
		}
		if !errors.Is(err, ErrInvalidPort) {
			t.Fatalf("expected ErrInvalidPort for target_port=%d, got %v", p, err)
		}
	}
}

func TestValidateForwardsEmptyTargetHost(t *testing.T) {
	fwd := []Forward{
		{ID: "a", ListenPort: 8080, TargetHost: "", TargetPort: 80},
	}
	if err := ValidateForwards(fwd); err == nil {
		t.Fatal("expected error for empty target_host, got nil")
	}

	fwd[0].TargetHost = "   "
	if err := ValidateForwards(fwd); err == nil {
		t.Fatal("expected error for whitespace-only target_host, got nil")
	}
}

func TestValidateForwardsDuplicateListenPort(t *testing.T) {
	fwd := []Forward{
		{ID: "a", ListenPort: 8080, TargetHost: "h1.svc", TargetPort: 80},
		{ID: "b", ListenPort: 8080, TargetHost: "h2.svc", TargetPort: 81},
	}
	if err := ValidateForwards(fwd); err == nil {
		t.Fatal("expected error for duplicate listen_port, got nil")
	}
}

func TestValidateForwardsValid(t *testing.T) {
	fwd := []Forward{
		{ID: "a", ListenPort: 8080, TargetHost: "h1.svc", TargetPort: 80, Enabled: true},
		{ID: "b", ListenPort: 8081, TargetHost: "h2.svc", TargetPort: 81, Enabled: false},
	}
	if err := ValidateForwards(fwd); err != nil {
		t.Fatalf("expected nil for valid forwards, got %v", err)
	}
}

func TestValidateForwardsEmptyList(t *testing.T) {
	if err := ValidateForwards(nil); err != nil {
		t.Fatalf("expected nil for nil list, got %v", err)
	}
	if err := ValidateForwards([]Forward{}); err != nil {
		t.Fatalf("expected nil for empty list, got %v", err)
	}
}
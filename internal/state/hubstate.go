package state

import (
	"fmt"
	"time"
)

// v2 hub/member state (docs/v2-proposal.md). The hub owner and a member owner
// are typically different parties on different machines, so their state is
// recorded separately: HubState is what the hub owner knows (the hub plus the
// members it has signed), MembershipState is what a member owner knows (its
// own membership of a remote hub). In the single-operator case both live in
// the same state file.

// PublishedEntry describes a member-local service published to the hub.
type PublishedEntry struct {
	// Name is the service name; the canonical authority is <Name>.<member>.
	Name string `json:"name"`
	// Namespace is the Kubernetes namespace of the backend service.
	Namespace string `json:"namespace,omitempty"`
	// Port is the backend service port.
	Port int `json:"port"`
	// Protocol is "http" or "grpc".
	Protocol string `json:"protocol"`
}

// RouteEntry describes a hub-side alias Service minted for a member service.
type RouteEntry struct {
	// Member is the member name.
	Member string `json:"member"`
	// Service is the published service name on the member.
	Service string `json:"service"`
	// AliasService is the ClusterIP Service name created on the hub
	// (e.g. inference-acme-prod in the portal namespace).
	AliasService string `json:"alias_service"`
}

// IssuedCert records a certificate previously issued to a member. Prior certs
// are retained until they expire so that eviction can revoke every valid cert
// a member holds — a renewed member keeps its old (still-valid, same-SAN)
// certificate until natural expiry, and that cert must also be revocable.
type IssuedCert struct {
	// Serial is the decimal serial number.
	Serial string `json:"serial"`
	// Expiry is the certificate NotAfter time; the record is prunable after it.
	Expiry time.Time `json:"expiry"`
}

// MemberRecord is the hub owner's record of a signed member.
type MemberRecord struct {
	// Name is the member name (certificate DNS SAN, reverse tunnel cluster-id).
	Name string `json:"name"`
	// Tenant is the tenant identifier the certificate was issued with.
	Tenant string `json:"tenant,omitempty"`
	// CertSerial is the decimal serial number of the member's current leaf
	// certificate, used to build CRL entries on eviction.
	CertSerial string `json:"cert_serial"`
	// CertExpiry is the NotAfter time of the current leaf certificate.
	CertExpiry time.Time `json:"cert_expiry,omitempty"`
	// PriorCerts are earlier certificates that have not yet expired. On
	// renewal the superseded cert moves here; eviction revokes all of them.
	PriorCerts []IssuedCert `json:"prior_certs,omitempty"`
	// Evicted marks the member as revoked; its serials are included in the CRL.
	Evicted bool `json:"evicted,omitempty"`
	// JoinedAt is when the member's certificate was first signed.
	JoinedAt time.Time `json:"joined_at"`
}

// HubState is the hub owner's record of a deployed hub.
type HubState struct {
	// Name is the hub name.
	Name string `json:"name"`
	// Context is the kubectl context of the hub cluster.
	Context string `json:"context"`
	// Namespace is the namespace holding hub components.
	Namespace string `json:"namespace"`
	// PublicAddr is the public tunnel endpoint (host:port) members dial.
	PublicAddr string `json:"public_addr"`
	// TunnelPort is the shared tunnel listener port.
	TunnelPort int `json:"tunnel_port"`
	// EgressPort is the egress listener port for hub-originated requests.
	EgressPort int `json:"egress_port"`
	// HandshakeSNI is the reserved SNI for reverse tunnel establishment.
	HandshakeSNI string `json:"handshake_sni"`
	// CADir is the directory holding the hub CA certificate and key.
	CADir string `json:"ca_dir"`
	// CRLNumber is a persisted monotonic counter for the CRL's Number field,
	// so successive CRLs strictly increase regardless of wall-clock changes.
	CRLNumber int64 `json:"crl_number,omitempty"`
	// EnvoyImage overrides the pinned Envoy image (empty = pinned default).
	EnvoyImage string `json:"envoy_image,omitempty"`
	// AllowUnsupportedEnvoy records that the version gate was bypassed.
	AllowUnsupportedEnvoy bool `json:"allow_unsupported_envoy,omitempty"`
	// CreatedAt is when the hub was initialized.
	CreatedAt time.Time `json:"created_at"`
	// Members are the members this hub has signed.
	Members []MemberRecord `json:"members,omitempty"`
	// Services are hub-side services published to members over the forward path.
	Services []ServiceEntry `json:"services,omitempty"`
	// Routes are alias Services minted for member services.
	Routes []RouteEntry `json:"routes,omitempty"`
}

// Member returns the record for the named member, or nil.
func (h *HubState) Member(name string) *MemberRecord {
	for i := range h.Members {
		if h.Members[i].Name == name {
			return &h.Members[i]
		}
	}
	return nil
}

// MembershipState is a member owner's record of its membership of a hub.
type MembershipState struct {
	// Member is this member's name.
	Member string `json:"member"`
	// Hub is the hub name (as embedded in the credential or given at join).
	Hub string `json:"hub"`
	// HubAddr is the hub tunnel endpoint (host:port).
	HubAddr string `json:"hub_addr"`
	// Context is the kubectl context of the member cluster.
	Context string `json:"context"`
	// Namespace is the namespace holding member components.
	Namespace string `json:"namespace"`
	// HandshakeSNI is the reserved SNI for reverse tunnel establishment.
	HandshakeSNI string `json:"handshake_sni"`
	// ConnectionCount is the number of reverse connections maintained.
	ConnectionCount int `json:"connection_count"`
	// EnvoyImage overrides the pinned Envoy image (empty = pinned default).
	EnvoyImage string `json:"envoy_image,omitempty"`
	// AllowUnsupportedEnvoy records that the version gate was bypassed.
	AllowUnsupportedEnvoy bool `json:"allow_unsupported_envoy,omitempty"`
	// Pending marks phase 1 of enrollment: key generated and CSR emitted,
	// certificate not yet installed.
	Pending bool `json:"pending,omitempty"`
	// JoinedAt is when the member joined (or started enrollment).
	JoinedAt time.Time `json:"joined_at"`
	// Published are the local services published to the hub.
	Published []PublishedEntry `json:"published,omitempty"`
	// Forward are v1-style forward services reachable from this member.
	Forward []ServiceEntry `json:"forward,omitempty"`
}

// PublishedService returns the published entry for the named service, or nil.
func (m *MembershipState) PublishedService(name string) *PublishedEntry {
	for i := range m.Published {
		if m.Published[i].Name == name {
			return &m.Published[i]
		}
	}
	return nil
}

// --- Store CRUD for hubs ---

// AddHub appends a hub record, failing if the name already exists.
func (s *Store) AddHub(h HubState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sf, err := s.loadLocked()
	if err != nil {
		return err
	}
	for _, existing := range sf.Hubs {
		if existing.Name == h.Name {
			return fmt.Errorf("hub %q already exists", h.Name)
		}
	}
	sf.Hubs = append(sf.Hubs, h)
	return s.saveLocked(sf)
}

// UpdateHub replaces the hub record with the same name.
func (s *Store) UpdateHub(h HubState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sf, err := s.loadLocked()
	if err != nil {
		return err
	}
	for i := range sf.Hubs {
		if sf.Hubs[i].Name == h.Name {
			sf.Hubs[i] = h
			return s.saveLocked(sf)
		}
	}
	return fmt.Errorf("hub %q not found", h.Name)
}

// RemoveHub deletes the named hub record.
func (s *Store) RemoveHub(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sf, err := s.loadLocked()
	if err != nil {
		return err
	}
	for i := range sf.Hubs {
		if sf.Hubs[i].Name == name {
			sf.Hubs = append(sf.Hubs[:i], sf.Hubs[i+1:]...)
			return s.saveLocked(sf)
		}
	}
	return fmt.Errorf("hub %q not found", name)
}

// GetHub returns the named hub record.
func (s *Store) GetHub(name string) (*HubState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sf, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	for i := range sf.Hubs {
		if sf.Hubs[i].Name == name {
			h := sf.Hubs[i]
			return &h, nil
		}
	}
	return nil, fmt.Errorf("hub %q not found", name)
}

// ListHubs returns all hub records.
func (s *Store) ListHubs() ([]HubState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sf, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	return append([]HubState(nil), sf.Hubs...), nil
}

// --- Store CRUD for memberships ---

// AddMembership appends a membership record, failing if the member name
// already exists.
func (s *Store) AddMembership(m MembershipState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sf, err := s.loadLocked()
	if err != nil {
		return err
	}
	for _, existing := range sf.Memberships {
		if existing.Member == m.Member {
			return fmt.Errorf("membership %q already exists", m.Member)
		}
	}
	sf.Memberships = append(sf.Memberships, m)
	return s.saveLocked(sf)
}

// UpdateMembership replaces the membership record with the same member name.
func (s *Store) UpdateMembership(m MembershipState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sf, err := s.loadLocked()
	if err != nil {
		return err
	}
	for i := range sf.Memberships {
		if sf.Memberships[i].Member == m.Member {
			sf.Memberships[i] = m
			return s.saveLocked(sf)
		}
	}
	return fmt.Errorf("membership %q not found", m.Member)
}

// RemoveMembership deletes the named membership record.
func (s *Store) RemoveMembership(member string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sf, err := s.loadLocked()
	if err != nil {
		return err
	}
	for i := range sf.Memberships {
		if sf.Memberships[i].Member == member {
			sf.Memberships = append(sf.Memberships[:i], sf.Memberships[i+1:]...)
			return s.saveLocked(sf)
		}
	}
	return fmt.Errorf("membership %q not found", member)
}

// GetMembership returns the named membership record.
func (s *Store) GetMembership(member string) (*MembershipState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sf, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	for i := range sf.Memberships {
		if sf.Memberships[i].Member == member {
			m := sf.Memberships[i]
			return &m, nil
		}
	}
	return nil, fmt.Errorf("membership %q not found", member)
}

// ListMemberships returns all membership records.
func (s *Store) ListMemberships() ([]MembershipState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sf, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	return append([]MembershipState(nil), sf.Memberships...), nil
}

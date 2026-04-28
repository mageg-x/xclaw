package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type AgentCard struct {
	ID           string    `json:"id,omitempty"`
	Name         string    `json:"name"`
	Description  string    `json:"description,omitempty"`
	Version      string    `json:"version"`
	Endpoint     string    `json:"endpoint"`
	Capabilities []string  `json:"capabilities"`
	Protocols    []string  `json:"protocols,omitempty"`
	Tools        []string  `json:"tools,omitempty"`
	TaskTypes    []string  `json:"task_types,omitempty"`
	LastSeenAt   time.Time `json:"last_seen_at"`
	Source       string    `json:"source"`
}

type Discovery struct {
	self        AgentCard
	registryURL string
	mu          sync.RWMutex
	peers       map[string]AgentCard
	cancel      context.CancelFunc
}

type multicastPacket struct {
	Service string    `json:"service"`
	Card    AgentCard `json:"card"`
}

func NewDiscovery(self AgentCard, registryURL string) *Discovery {
	return &Discovery{
		self:        self,
		registryURL: strings.TrimSpace(registryURL),
		peers:       make(map[string]AgentCard),
	}
}

func (d *Discovery) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	d.cancel = cancel
	go d.runMulticastPublisher(ctx)
	go d.runMulticastListener(ctx)
	go d.runRegistrySync(ctx)
}

func (d *Discovery) Close() {
	if d.cancel != nil {
		d.cancel()
	}
}

func (d *Discovery) ListPeers() []AgentCard {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().UTC()
	out := make([]AgentCard, 0, len(d.peers))
	for key, peer := range d.peers {
		if now.Sub(peer.LastSeenAt) > 5*time.Minute {
			delete(d.peers, key)
			continue
		}
		out = append(out, peer)
	}
	return out
}

func (d *Discovery) runMulticastPublisher(ctx context.Context) {
	addr, err := net.ResolveUDPAddr("udp4", "224.0.0.251:5353")
	if err != nil {
		return
	}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return
	}
	defer conn.Close()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		d.broadcastOnce(conn)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Discovery) broadcastOnce(conn *net.UDPConn) {
	card := d.self
	card.LastSeenAt = time.Now().UTC()
	card.Source = "mdns"
	payload, err := json.Marshal(multicastPacket{Service: "xclaw-a2a", Card: card})
	if err != nil {
		return
	}
	_, _ = conn.Write(payload)
}

func (d *Discovery) runMulticastListener(ctx context.Context) {
	addr, err := net.ResolveUDPAddr("udp4", "224.0.0.251:5353")
	if err != nil {
		return
	}
	conn, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		return
	}
	_ = conn.SetReadBuffer(64 * 1024)
	defer conn.Close()

	buf := make([]byte, 64*1024)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				select {
				case <-ctx.Done():
					return
				default:
				}
				continue
			}
			return
		}
		var packet multicastPacket
		if err := json.Unmarshal(buf[:n], &packet); err != nil {
			continue
		}
		if packet.Service != "xclaw-a2a" {
			continue
		}
		if packet.Card.Endpoint == "" || packet.Card.Endpoint == d.self.Endpoint {
			continue
		}
		packet.Card.LastSeenAt = time.Now().UTC()
		packet.Card.Source = "mdns"
		d.mu.Lock()
		d.peers[packet.Card.Endpoint] = packet.Card
		d.mu.Unlock()
	}
}

func (d *Discovery) runRegistrySync(ctx context.Context) {
	if d.registryURL == "" {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	for {
		d.syncRegistryOnce(ctx, client)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Discovery) syncRegistryOnce(ctx context.Context, client *http.Client) {
	registerURL := strings.TrimRight(d.registryURL, "/") + "/register"
	body, _ := json.Marshal(d.self)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, registerURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	_, _ = client.Do(req)

	listURL := strings.TrimRight(d.registryURL, "/") + "/peers"
	listReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	resp, err := client.Do(listReq)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return
	}
	var peers []AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&peers); err != nil {
		return
	}
	now := time.Now().UTC()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, peer := range peers {
		if peer.Endpoint == "" || peer.Endpoint == d.self.Endpoint {
			continue
		}
		peer.Source = "registry"
		if peer.LastSeenAt.IsZero() {
			peer.LastSeenAt = now
		}
		d.peers[peer.Endpoint] = peer
	}
}

func (d *Discovery) SelfCard() AgentCard {
	return d.self
}

func DefaultLocalCard(host string, port int, tlsEnabled bool) AgentCard {
	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	return AgentCard{
		ID:           "xclaw-local-node",
		Name:         "xclaw-local-node",
		Description:  "XClaw local A2A node",
		Version:      "1.0.0",
		Endpoint:     fmt.Sprintf("%s://%s:%d/a2a", scheme, host, port),
		Capabilities: []string{"task", "message", "result", "mcp"},
		Protocols:    []string{"a2a", "http", "sse", "websocket", "mcp"},
		TaskTypes:    []string{"delegated-task"},
		LastSeenAt:   time.Now().UTC(),
		Source:       "self",
	}
}

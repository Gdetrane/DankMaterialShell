package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	defaultSocketPath = "/var/run/tailscale/tailscaled.sock"
	statusEndpoint    = "/localapi/v0/status"
	fetchTimeout      = 10 * time.Second
)

// newSocketHTTPClient creates an HTTP client that communicates over a Unix socket.
func newSocketHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: fetchTimeout,
	}
}

// fetchStatusWithClient fetches the Tailscale status from the given URL using the provided HTTP client.
func fetchStatusWithClient(client *http.Client, url string) (*TailscaleState, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch tailscale status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tailscale API returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return parseStatusResponse(raw)
}

// parseStatusResponse converts the raw JSON map from the Tailscale local API into a TailscaleState.
func parseStatusResponse(raw map[string]any) (*TailscaleState, error) {
	backendState, _ := raw["BackendState"].(string)
	connected := backendState == "Running"

	state := &TailscaleState{
		Connected:    connected,
		BackendState: backendState,
	}

	if !connected {
		return state, nil
	}

	state.Version, _ = raw["Version"].(string)
	state.MagicDNSSuffix, _ = raw["MagicDNSSuffix"].(string)

	if tailnet, ok := raw["CurrentTailnet"].(map[string]any); ok {
		state.TailnetName, _ = tailnet["Name"].(string)
	}

	// Build user lookup map
	users := make(map[float64]string)
	if userMap, ok := raw["User"].(map[string]any); ok {
		for _, u := range userMap {
			if user, ok := u.(map[string]any); ok {
				if id, ok := user["ID"].(float64); ok {
					loginName, _ := user["LoginName"].(string)
					users[id] = loginName
				}
			}
		}
	}

	// Parse self
	if self, ok := raw["Self"].(map[string]any); ok {
		state.Self = parsePeer(self, users)
	}

	// Parse peers
	if peerMap, ok := raw["Peer"].(map[string]any); ok {
		peers := make([]Peer, 0, len(peerMap))
		for _, p := range peerMap {
			if peerData, ok := p.(map[string]any); ok {
				peers = append(peers, parsePeer(peerData, users))
			}
		}
		sort.Slice(peers, func(i, j int) bool {
			if peers[i].Online != peers[j].Online {
				return peers[i].Online
			}
			return strings.ToLower(peers[i].Hostname) < strings.ToLower(peers[j].Hostname)
		})
		state.Peers = peers
	}

	return state, nil
}

// parsePeer extracts a Peer from a raw JSON map, resolving user IDs to login names.
func parsePeer(data map[string]any, users map[float64]string) Peer {
	peer := Peer{}

	peer.ID, _ = data["ID"].(string)
	peer.Hostname, _ = data["HostName"].(string)
	if dnsName, ok := data["DNSName"].(string); ok {
		peer.DNSName = strings.TrimSuffix(dnsName, ".")
	}
	// Mobile devices report "localhost" as hostname — use DNSName instead
	if (peer.Hostname == "" || peer.Hostname == "localhost") && peer.DNSName != "" {
		parts := strings.SplitN(peer.DNSName, ".", 2)
		if len(parts) > 0 {
			peer.Hostname = parts[0]
		}
	}
	peer.OS, _ = data["OS"].(string)
	peer.Online, _ = data["Online"].(bool)
	peer.Active, _ = data["Active"].(bool)
	peer.ExitNode, _ = data["ExitNode"].(bool)
	peer.Relay, _ = data["Relay"].(string)

	if rxBytes, ok := data["RxBytes"].(float64); ok {
		peer.RxBytes = int64(rxBytes)
	}
	if txBytes, ok := data["TxBytes"].(float64); ok {
		peer.TxBytes = int64(txBytes)
	}

	if ips, ok := data["TailscaleIPs"].([]any); ok {
		for _, ip := range ips {
			if ipStr, ok := ip.(string); ok {
				if strings.Contains(ipStr, ":") {
					if peer.TailscaleIPv6 == "" {
						peer.TailscaleIPv6 = ipStr
					}
				} else {
					if peer.TailscaleIP == "" {
						peer.TailscaleIP = ipStr
					}
				}
			}
		}
	}

	if tags, ok := data["Tags"].([]any); ok {
		for _, tag := range tags {
			if tagStr, ok := tag.(string); ok {
				peer.Tags = append(peer.Tags, tagStr)
			}
		}
	}

	if userID, ok := data["UserID"].(float64); ok && userID > 0 {
		peer.Owner = users[userID]
	}

	if lastSeen, ok := data["LastSeen"].(string); ok && lastSeen != "" && lastSeen != "0001-01-01T00:00:00Z" {
		if t, err := time.Parse(time.RFC3339, lastSeen); err == nil {
			peer.LastSeen = formatRelativeTime(t)
		}
	}

	return peer
}

// formatRelativeTime formats a time as a human-readable relative duration (e.g., "5 minutes ago").
func formatRelativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}

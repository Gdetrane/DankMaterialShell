package tailscale

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleStatusResponse = `{
	"Version": "1.94.2",
	"BackendState": "Running",
	"TUN": true,
	"HaveNodeKey": true,
	"TailscaleIPs": ["100.85.254.40", "fd7a:115c:a1e0::1"],
	"Self": {
		"ID": "node1",
		"HostName": "cachyos",
		"DNSName": "cachyos.example.ts.net.",
		"OS": "linux",
		"TailscaleIPs": ["100.85.254.40", "fd7a:115c:a1e0::1"],
		"Online": true,
		"UserID": 12345
	},
	"MagicDNSSuffix": "example.ts.net",
	"CurrentTailnet": {
		"Name": "user@example.com",
		"MagicDNSSuffix": "example.ts.net"
	},
	"Peer": {
		"key1": {
			"ID": "node2",
			"HostName": "thinkpad-x390",
			"DNSName": "thinkpad-x390.example.ts.net.",
			"OS": "linux",
			"TailscaleIPs": ["100.97.21.17", "fd7a:115c:a1e0::2"],
			"Online": true,
			"Active": true,
			"Relay": "fra",
			"RxBytes": 1024,
			"TxBytes": 2048,
			"UserID": 12345,
			"ExitNode": false,
			"LastSeen": "2026-03-01T12:00:00Z"
		},
		"key2": {
			"ID": "node3",
			"HostName": "k8s-node",
			"DNSName": "k8s-node.example.ts.net.",
			"OS": "linux",
			"TailscaleIPs": ["100.100.100.1"],
			"Online": false,
			"Active": false,
			"Tags": ["tag:k8s"],
			"UserID": 0,
			"LastSeen": "2026-02-28T10:00:00Z"
		}
	},
	"User": {
		"12345": {
			"ID": 12345,
			"LoginName": "user@example.com",
			"DisplayName": "User"
		}
	}
}`

func TestParseStatusResponse(t *testing.T) {
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(sampleStatusResponse), &raw))

	state, err := parseStatusResponse(raw)
	require.NoError(t, err)

	assert.True(t, state.Connected)
	assert.Equal(t, "1.94.2", state.Version)
	assert.Equal(t, "Running", state.BackendState)
	assert.Equal(t, "example.ts.net", state.MagicDNSSuffix)
	assert.Equal(t, "user@example.com", state.TailnetName)

	// Self
	assert.Equal(t, "cachyos", state.Self.Hostname)
	assert.Equal(t, "cachyos.example.ts.net", state.Self.DNSName)
	assert.Equal(t, "100.85.254.40", state.Self.TailscaleIP)
	assert.Equal(t, "fd7a:115c:a1e0::1", state.Self.TailscaleIPv6)
	assert.Equal(t, "linux", state.Self.OS)
	assert.True(t, state.Self.Online)

	// Peers
	assert.Len(t, state.Peers, 2)

	var onlinePeer, offlinePeer Peer
	for _, p := range state.Peers {
		if p.Hostname == "thinkpad-x390" {
			onlinePeer = p
		}
		if p.Hostname == "k8s-node" {
			offlinePeer = p
		}
	}

	assert.True(t, onlinePeer.Online)
	assert.Equal(t, "100.97.21.17", onlinePeer.TailscaleIP)
	assert.Equal(t, "fra", onlinePeer.Relay)
	assert.Equal(t, "user@example.com", onlinePeer.Owner)
	assert.Equal(t, int64(1024), onlinePeer.RxBytes)

	assert.False(t, offlinePeer.Online)
	assert.Equal(t, "k8s-node", offlinePeer.Hostname)
	assert.Contains(t, offlinePeer.Tags, "tag:k8s")
	assert.Equal(t, "", offlinePeer.Owner)
}

func TestParseStatusResponse_NotRunning(t *testing.T) {
	raw := map[string]any{
		"BackendState": "Stopped",
	}

	state, err := parseStatusResponse(raw)
	require.NoError(t, err)
	assert.False(t, state.Connected)
	assert.Empty(t, state.Peers)
}

func TestParseStatusResponse_Empty(t *testing.T) {
	raw := map[string]any{}

	state, err := parseStatusResponse(raw)
	require.NoError(t, err)
	assert.False(t, state.Connected)
}

func TestFetchStatus_HTTPServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/localapi/v0/status", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(sampleStatusResponse))
	}))
	defer server.Close()

	client := &http.Client{}
	state, err := fetchStatusWithClient(client, server.URL+"/localapi/v0/status")
	require.NoError(t, err)
	assert.True(t, state.Connected)
	assert.Equal(t, "cachyos", state.Self.Hostname)
	assert.Len(t, state.Peers, 2)
}

func TestFetchStatus_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := &http.Client{}
	_, err := fetchStatusWithClient(client, server.URL+"/localapi/v0/status")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
}

func TestPeerSorting(t *testing.T) {
	// Verify online peers come before offline, then alphabetical
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(sampleStatusResponse), &raw))

	state, err := parseStatusResponse(raw)
	require.NoError(t, err)

	// thinkpad-x390 is online, k8s-node is offline
	// Online should come first
	assert.Equal(t, "thinkpad-x390", state.Peers[0].Hostname)
	assert.Equal(t, "k8s-node", state.Peers[1].Hostname)
}

func TestFormatRelativeTime(t *testing.T) {
	tests := []struct {
		name     string
		duration string
		contains string
	}{
		{"minutes", "5m", "minutes ago"},
		{"hours", "3h", "hours ago"},
		{"days", "48h", "days ago"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := time.ParseDuration(tt.duration)
			result := formatRelativeTime(time.Now().Add(-d))
			assert.Contains(t, result, tt.contains)
		})
	}
}

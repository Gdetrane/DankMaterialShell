package tailscale

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewManager_SocketNotFound(t *testing.T) {
	_, err := NewManager("/tmp/nonexistent-tailscale-test.sock")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tailscale socket not found")
}

func TestManager_GetState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(sampleStatusResponse))
	}))
	defer server.Close()

	m := newTestManager(server.URL)
	defer m.Close()

	err := m.poll()
	require.NoError(t, err)

	state := m.GetState()
	assert.True(t, state.Connected)
	assert.Equal(t, "cachyos", state.Self.Hostname)
	assert.Len(t, state.Peers, 2)
	assert.Equal(t, "1.94.2", state.Version)
	assert.Equal(t, "Running", state.BackendState)
}

func TestManager_Subscribe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(sampleStatusResponse))
	}))
	defer server.Close()

	m := newTestManager(server.URL)
	defer m.Close()

	err := m.poll()
	require.NoError(t, err)

	ch := m.Subscribe("test-client-1")
	assert.NotNil(t, ch)

	// Verify a second subscriber also works
	ch2 := m.Subscribe("test-client-2")
	assert.NotNil(t, ch2)

	// Unsubscribe first client
	m.Unsubscribe("test-client-1")

	// Unsubscribe second client
	m.Unsubscribe("test-client-2")
}

func TestManager_Close(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(sampleStatusResponse))
	}))
	defer server.Close()

	m := newTestManager(server.URL)

	// Subscribe before closing
	ch := m.Subscribe("test-client")
	assert.NotNil(t, ch)

	// Close should not panic
	assert.NotPanics(t, func() {
		m.Close()
	})
}

func TestStateChanged(t *testing.T) {
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(sampleStatusResponse), &raw))

	state1, err := parseStatusResponse(raw)
	require.NoError(t, err)

	// nil vs state should be changed
	assert.True(t, stateChanged(nil, state1))

	// Same state should not be changed
	state2 := *state1
	assert.False(t, stateChanged(state1, &state2))

	// Modified state should be changed
	state3 := *state1
	state3.BackendState = "Stopped"
	state3.Connected = false
	assert.True(t, stateChanged(state1, &state3))

	// Different peer count should be changed
	state4 := *state1
	state4.Peers = state4.Peers[:1]
	assert.True(t, stateChanged(state1, &state4))
}

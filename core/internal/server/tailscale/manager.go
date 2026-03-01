package tailscale

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/AvengeMedia/DankMaterialShell/core/internal/log"
	"github.com/AvengeMedia/DankMaterialShell/core/pkg/syncmap"
)

const pollInterval = 30 * time.Second

// Manager manages Tailscale state polling and subscriber notifications.
type Manager struct {
	state       *TailscaleState
	stateMutex  sync.RWMutex
	subscribers syncmap.Map[string, chan TailscaleState]
	stopChan    chan struct{}
	pollWG      sync.WaitGroup
	notifierWg  sync.WaitGroup
	socketPath  string
	lastState   *TailscaleState
	httpClient  *http.Client
	statusURL   string
}

// NewManager creates a new Tailscale manager. It checks that the socket exists,
// performs an initial poll, and starts background polling.
func NewManager(socketPath string) (*Manager, error) {
	if socketPath == "" {
		socketPath = defaultSocketPath
	}
	if _, err := os.Stat(socketPath); err != nil {
		return nil, fmt.Errorf("tailscale socket not found at %s: %w", socketPath, err)
	}

	client := newSocketHTTPClient(socketPath)
	statusURL := "http://local-tailscaled.sock" + statusEndpoint

	m := &Manager{
		state:      &TailscaleState{},
		socketPath: socketPath,
		httpClient: client,
		statusURL:  statusURL,
		stopChan:   make(chan struct{}),
	}

	if err := m.poll(); err != nil {
		log.Warnf("[Tailscale] Initial poll failed: %v", err)
	}

	m.pollWG.Add(1)
	go m.pollLoop()

	return m, nil
}

// newTestManager creates a manager pointing at an httptest server URL instead of a Unix socket.
func newTestManager(baseURL string) *Manager {
	return &Manager{
		state:      &TailscaleState{},
		httpClient: &http.Client{},
		statusURL:  baseURL + statusEndpoint,
		stopChan:   make(chan struct{}),
	}
}

// poll fetches the current Tailscale status and updates the manager state.
func (m *Manager) poll() error {
	state, err := fetchStatusWithClient(m.httpClient, m.statusURL)
	if err != nil {
		return err
	}

	m.stateMutex.Lock()
	m.state = state
	m.stateMutex.Unlock()

	return nil
}

// pollLoop runs on a ticker, polling Tailscale every pollInterval and notifying subscribers if state changed.
func (m *Manager) pollLoop() {
	defer m.pollWG.Done()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			if err := m.poll(); err != nil {
				log.Warnf("[Tailscale] Poll failed: %v", err)
				// Mark as disconnected but keep polling so we recover
				// when the daemon restarts
				m.stateMutex.Lock()
				m.state = &TailscaleState{Connected: false, BackendState: "Unreachable"}
				m.stateMutex.Unlock()
			}
			m.checkAndNotify()
		}
	}
}

// checkAndNotify compares the current state with the last notified state and broadcasts if changed.
func (m *Manager) checkAndNotify() {
	m.stateMutex.RLock()
	current := m.state
	m.stateMutex.RUnlock()

	if stateChanged(m.lastState, current) {
		stateCopy := *current
		m.lastState = &stateCopy
		m.broadcastState(*current)
	}
}

// broadcastState sends the given state to all subscriber channels.
func (m *Manager) broadcastState(state TailscaleState) {
	m.subscribers.Range(func(key string, ch chan TailscaleState) bool {
		select {
		case ch <- state:
		default:
		}
		return true
	})
}

// GetState returns a copy of the current Tailscale state.
func (m *Manager) GetState() TailscaleState {
	m.stateMutex.RLock()
	defer m.stateMutex.RUnlock()

	if m.state == nil {
		return TailscaleState{}
	}
	return *m.state
}

// Subscribe creates a buffered channel for the given client ID and stores it.
func (m *Manager) Subscribe(clientID string) chan TailscaleState {
	ch := make(chan TailscaleState, 64)
	m.subscribers.Store(clientID, ch)
	return ch
}

// Unsubscribe removes and closes the subscriber channel for the given client ID.
func (m *Manager) Unsubscribe(clientID string) {
	if val, ok := m.subscribers.LoadAndDelete(clientID); ok {
		close(val)
	}
}

// Close stops the polling goroutine and closes all subscriber channels.
func (m *Manager) Close() {
	close(m.stopChan)
	m.pollWG.Wait()
	m.notifierWg.Wait()

	m.subscribers.Range(func(key string, ch chan TailscaleState) bool {
		close(ch)
		m.subscribers.Delete(key)
		return true
	})
}

// RefreshState triggers an immediate poll and notifies subscribers if state changed.
func (m *Manager) RefreshState() {
	if err := m.poll(); err != nil {
		log.Warnf("[Tailscale] Failed to refresh state: %v", err)
		return
	}
	m.checkAndNotify()
}

// stateChanged compares two states using JSON serialization.
func stateChanged(old, new *TailscaleState) bool {
	if old == nil && new == nil {
		return false
	}
	if old == nil || new == nil {
		return true
	}

	oldJSON, err := json.Marshal(old)
	if err != nil {
		return true
	}
	newJSON, err := json.Marshal(new)
	if err != nil {
		return true
	}

	return string(oldJSON) != string(newJSON)
}

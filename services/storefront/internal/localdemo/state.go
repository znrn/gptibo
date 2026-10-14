// Package localdemo is used exclusively by cmd/localdemo. Production commands
// never import it or select its simulated providers through environment flags.
package localdemo

import (
	"encoding/json"
	"errors"
	"gptibo/storefront/internal/recharge"
	"os"
	"path/filepath"
	"sync"
)

type Letter struct {
	ID      string `json:"id"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
	Code    string `json:"code,omitempty"`
	Time    string `json:"time"`
}
type Snapshot struct {
	Letters  []Letter                         `json:"letters"`
	Orders   map[string]recharge.RemoteOrder  `json:"orders"`
	Attempts map[string]recharge.Redemption   `json:"attempts"`
	Refunds  map[string]recharge.RemoteRefund `json:"refunds"`
	Receipts map[string]string                `json:"receipts"`
	Payments map[string]string                `json:"payments"`
}
type State struct {
	sync.Mutex
	Snapshot
	path string
}

func OpenState(path string) (*State, error) {
	s := &State{path: path}
	b, e := os.ReadFile(path)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	if len(b) > 0 {
		if e = json.Unmarshal(b, &s.Snapshot); e != nil {
			return nil, e
		}
	}
	if s.Orders == nil {
		s.Orders = map[string]recharge.RemoteOrder{}
	}
	if s.Attempts == nil {
		s.Attempts = map[string]recharge.Redemption{}
	}
	if s.Refunds == nil {
		s.Refunds = map[string]recharge.RemoteRefund{}
	}
	if s.Receipts == nil {
		s.Receipts = map[string]string{}
	}
	if s.Payments == nil {
		s.Payments = map[string]string{}
	}
	return s, nil
}

// Call with the lock held. Atomic replacement keeps supplier idempotency across restarts.
func (s *State) save() error {
	b, e := json.Marshal(s.Snapshot)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(s.path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(s.path), ".demo-state-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), s.path)
}

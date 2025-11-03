package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

var ErrNotFound = errors.New("machine not found")

type Status struct {
	ID          string    `json:"id"`
	State       string    `json:"state"`
	SocketPath  string    `json:"socket_path"`
	VolumePath  string    `json:"volume_path"`
	LogFile     string    `json:"log_file"`
	LogFifo     string    `json:"log_fifo"`
	MetricsFifo string    `json:"metrics_fifo"`
	TapDevice   string    `json:"tap_device,omitempty"`
	MacAddress  string    `json:"mac_address,omitempty"`
	HostIPCIDR  string    `json:"host_ip_cidr,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Error       string    `json:"error,omitempty"`
}

func LoadStatus(path string) (*Status, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read status file: %w", err)
	}

	var st Status
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse status file: %w", err)
	}
	return &st, nil
}

func (s *Status) Save(path string) error {
	if s == nil {
		return errors.New("nil status")
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode status: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write status file: %w", err)
	}
	return nil
}

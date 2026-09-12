package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"time"
)

const managedUpdaterSocket = "/run/sub2api-update.sock"
const managedUpdaterStatus = "/var/lib/sub2api-updater/status.json"

type ManagedUpdateStatus struct {
	Phase     string `json:"phase"`
	Version   string `json:"version,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

func (s *UpdateService) updaterAvailable() bool {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return false
	}
	info, err := os.Stat(s.updaterSocket)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

func (s *UpdateService) requestManagedUpdate(ctx context.Context, version string) error {
	if !budVersionPattern.MatchString(version) || !strings.Contains(version, "-Bud.") {
		return fmt.Errorf("invalid Bud version")
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", s.updaterSocket)
	if err != nil {
		return fmt.Errorf("Bud installer unavailable")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(map[string]string{"version": version}); err != nil {
		return fmt.Errorf("cannot submit update")
	}
	var reply struct {
		Accepted bool `json:"accepted"`
	}
	if err := json.NewDecoder(io.LimitReader(conn, 1024)).Decode(&reply); err != nil {
		return fmt.Errorf("update acknowledgement unavailable; check update status before retrying")
	}
	if !reply.Accepted {
		return fmt.Errorf("update rejected or another installation is active; check update status")
	}
	return nil
}

func (s *UpdateService) GetUpdateStatus(context.Context) (*ManagedUpdateStatus, error) {
	f, err := os.Open(s.updaterStatus)
	if os.IsNotExist(err) {
		return &ManagedUpdateStatus{Phase: "idle"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("update status unavailable")
	}
	defer f.Close()
	var status ManagedUpdateStatus
	if err := json.NewDecoder(io.LimitReader(f, 4096)).Decode(&status); err != nil {
		return nil, fmt.Errorf("invalid update status")
	}
	return &status, nil
}

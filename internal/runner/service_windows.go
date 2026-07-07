//go:build windows

package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const windowsServiceName = "FixForgeClient"

func maybeRunPlatformService(cfg *Config, logger *slog.Logger) (bool, error) {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return false, err
	}
	if !isService {
		return false, nil
	}
	return true, svc.Run(windowsServiceName, &windowsRunnerService{cfg: cfg, logger: logger})
}

type windowsRunnerService struct {
	cfg    *Config
	logger *slog.Logger
}

func (s *windowsRunnerService) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- runDaemons(ctx, s.cfg, s.logger)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case err := <-errCh:
			if err != nil && err != context.Canceled {
				return false, 1
			}
			return false, 0
		case req := <-requests:
			switch req.Cmd {
			case svc.Interrogate:
				changes <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				if err := <-errCh; err != nil && err != context.Canceled {
					return false, 1
				}
				return false, 0
			default:
				continue
			}
		}
	}
}

func DoServiceInstall() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	cfgPath := DefaultConfigPath()
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()

	serviceConfig := mgr.Config{
		DisplayName:    "FixForge Client",
		Description:    "FixForge local client daemon",
		StartType:      mgr.StartAutomatic,
		BinaryPathName: windowsServiceBinaryPath(exe, cfgPath),
	}
	if existing, err := m.OpenService(windowsServiceName); err == nil {
		defer existing.Close()
		if err := existing.UpdateConfig(serviceConfig); err != nil {
			return fmt.Errorf("update service: %w", err)
		}
		if err := existing.Start(); err != nil && err != windows.ERROR_SERVICE_ALREADY_RUNNING {
			return fmt.Errorf("start service: %w", err)
		}
		fmt.Println("Service updated: FixForgeClient")
		return nil
	}

	s, err := m.CreateService(windowsServiceName, exe, mgr.Config{
		DisplayName: "FixForge Client",
		Description: "FixForge local client daemon",
		StartType:   mgr.StartAutomatic,
	}, "run", "--config", cfgPath)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()
	if err := s.Start(); err != nil && err != windows.ERROR_SERVICE_ALREADY_RUNNING {
		return fmt.Errorf("start service: %w", err)
	}
	fmt.Println("Service installed: FixForgeClient")
	return nil
}

func DoServiceUninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(windowsServiceName)
	if err != nil {
		if err == windows.ERROR_SERVICE_DOES_NOT_EXIST {
			return nil
		}
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()
	_ = stopWindowsService(s)
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	fmt.Println("Service uninstalled: FixForgeClient")
	return nil
}

func DoServiceStart() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(windowsServiceName)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()
	if err := s.Start(); err != nil && err != windows.ERROR_SERVICE_ALREADY_RUNNING {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}

func DoServiceStop() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(windowsServiceName)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()
	return stopWindowsService(s)
}

func DoServiceStatus() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(windowsServiceName)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()
	status, err := s.Query()
	if err != nil {
		return fmt.Errorf("query service: %w", err)
	}
	fmt.Printf("Service: %s\nState:   %s\n", windowsServiceName, windowsServiceState(status.State))
	return nil
}

func stopWindowsService(s *mgr.Service) error {
	status, err := s.Query()
	if err != nil {
		return err
	}
	if status.State == svc.Stopped {
		return nil
	}
	status, err = s.Control(svc.Stop)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(20 * time.Second)
	for status.State != svc.Stopped {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for service stop")
		}
		time.Sleep(300 * time.Millisecond)
		status, err = s.Query()
		if err != nil {
			return err
		}
	}
	return nil
}

func windowsServiceState(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start pending"
	case svc.StopPending:
		return "stop pending"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continue pending"
	case svc.PausePending:
		return "pause pending"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("unknown(%d)", state)
	}
}

func windowsServiceBinaryPath(exe, cfgPath string) string {
	return windowsCommandArg(exe) + " run --config " + windowsCommandArg(cfgPath)
}

func windowsCommandArg(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}

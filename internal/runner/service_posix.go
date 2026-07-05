//go:build !windows

package runner

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const systemdServiceName = "fixforge-client.service"
const launchAgentLabel = "com.fixforge.client"

func maybeRunPlatformService(_ *Config, _ *slog.Logger) (bool, error) {
	return false, nil
}

func DoServiceInstall() error {
	switch runtime.GOOS {
	case "linux":
		return installSystemdUserService()
	case "darwin":
		return installLaunchAgent()
	default:
		return fmt.Errorf("service install is not supported on %s", runtime.GOOS)
	}
}

func DoServiceUninstall() error {
	switch runtime.GOOS {
	case "linux":
		return uninstallSystemdUserService()
	case "darwin":
		return uninstallLaunchAgent()
	default:
		return fmt.Errorf("service uninstall is not supported on %s", runtime.GOOS)
	}
}

func DoServiceStart() error {
	switch runtime.GOOS {
	case "linux":
		return runCommand("systemctl", "--user", "start", systemdServiceName)
	case "darwin":
		return launchctlKickstart()
	default:
		return fmt.Errorf("service start is not supported on %s", runtime.GOOS)
	}
}

func DoServiceStop() error {
	switch runtime.GOOS {
	case "linux":
		return runCommand("systemctl", "--user", "stop", systemdServiceName)
	case "darwin":
		return launchctlBootout()
	default:
		return fmt.Errorf("service stop is not supported on %s", runtime.GOOS)
	}
}

func DoServiceStatus() error {
	switch runtime.GOOS {
	case "linux":
		return runCommand("systemctl", "--user", "status", "--no-pager", systemdServiceName)
	case "darwin":
		if err := runCommand("launchctl", "print", launchctlDomain()+"/"+launchAgentLabel); err != nil {
			return runCommand("launchctl", "list", launchAgentLabel)
		}
		return nil
	default:
		return fmt.Errorf("service status is not supported on %s", runtime.GOOS)
	}
}

func installSystemdUserService() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return fmt.Errorf("create systemd unit dir: %w", err)
	}
	pathValue := os.Getenv("PATH")
	if pathValue == "" {
		pathValue = "/usr/local/bin:/usr/bin:/bin"
	}
	unit := fmt.Sprintf(`[Unit]
Description=FixForge Client
After=network-online.target

[Service]
Type=simple
ExecStart=%s run
Restart=always
RestartSec=5
WorkingDirectory=%s
Environment=%s
Environment=%s

[Install]
WantedBy=default.target
`, systemdCommandArg(exe), systemdPathValue(home), systemdEnvironmentAssignment("HOME", home), systemdEnvironmentAssignment("PATH", pathValue))
	unitPath := filepath.Join(unitDir, systemdServiceName)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write service unit: %w", err)
	}
	if err := runCommand("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := runCommand("systemctl", "--user", "enable", "--now", systemdServiceName); err != nil {
		return err
	}
	fmt.Printf("Service installed: %s\n", unitPath)
	return nil
}

func uninstallSystemdUserService() error {
	_ = runCommand("systemctl", "--user", "disable", "--now", systemdServiceName)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		_ = os.Remove(filepath.Join(home, ".config", "systemd", "user", systemdServiceName))
	}
	return runCommand("systemctl", "--user", "daemon-reload")
}

func installLaunchAgent() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	agentDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	logDir := filepath.Join(home, runnerConfigDirName)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	plistPath := filepath.Join(agentDir, launchAgentLabel+".plist")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>WorkingDirectory</key>
  <string>%s</string>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, xmlText(launchAgentLabel), xmlText(exe), xmlText(home), xmlText(filepath.Join(logDir, "runner.out.log")), xmlText(filepath.Join(logDir, "runner.err.log")))
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write launch agent: %w", err)
	}
	_ = runCommand("launchctl", "unload", plistPath)
	if err := runCommand("launchctl", "load", "-w", plistPath); err != nil {
		if bootErr := runCommand("launchctl", "bootstrap", launchctlDomain(), plistPath); bootErr != nil {
			return err
		}
	}
	_ = launchctlKickstart()
	fmt.Printf("Service installed: %s\n", plistPath)
	return nil
}

func uninstallLaunchAgent() error {
	_ = launchctlBootout()
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		plistPath := filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
		_ = runCommand("launchctl", "unload", plistPath)
		_ = os.Remove(plistPath)
	}
	return nil
}

func launchctlKickstart() error {
	return runCommand("launchctl", "kickstart", "-k", launchctlDomain()+"/"+launchAgentLabel)
}

func launchctlBootout() error {
	return runCommand("launchctl", "bootout", launchctlDomain()+"/"+launchAgentLabel)
}

func launchctlDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func systemdCommandArg(value string) string {
	return strconv.Quote(systemdEscapeSpecifiers(value))
}

func systemdEnvironmentAssignment(key, value string) string {
	return strconv.Quote(systemdEscapeSpecifiers(key + "=" + value))
}

func systemdPathValue(value string) string {
	escaped := systemdEscapeSpecifiers(value)
	var b strings.Builder
	b.Grow(len(escaped))
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		if isSystemdUnquotedPathByte(c) {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "\\x%02x", c)
	}
	return b.String()
}

func systemdEscapeSpecifiers(value string) string {
	return strings.ReplaceAll(value, "%", "%%")
}

func isSystemdUnquotedPathByte(c byte) bool {
	return c >= 'A' && c <= 'Z' ||
		c >= 'a' && c <= 'z' ||
		c >= '0' && c <= '9' ||
		c == '/' || c == '.' || c == '-' || c == '_' ||
		c == ':' || c == '@' || c == '+' || c == '=' ||
		c == ',' || c == '%'
}

func xmlText(value string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		fmt.Print(string(out))
	}
	if err != nil {
		return fmt.Errorf("%s %s failed: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

# fixforge-client

FixForge Client is the local daemon used by FixForge for project QA, workspace file operations, terminal proxying, and CloudRun execution.

## Install

```bash
curl -fsSL https://github.com/HobbyBear/fixforge-client/releases/latest/download/install.sh | bash
```

Linux/macOS installs to `~/.local/bin/fixforge-client` by default and updates shell startup files so new terminals can run `fixforge-client` directly.

Windows PowerShell:

```powershell
iwr -useb https://github.com/HobbyBear/fixforge-client/releases/latest/download/install.ps1 | iex
```

Windows installs to `%LOCALAPPDATA%\FixForge\bin\fixforge-client.exe` and adds that directory to the user `Path`.

Install and connect a project in one command:

```bash
curl -fsSL https://github.com/HobbyBear/fixforge-client/releases/latest/download/install.sh | bash -s -- connect \
  --server http://fixforge.example.com \
  --token <runner_token> \
  --project-id <project_name> \
  --project-name <project_name> \
  --repo-url git@gitlab.example.com:group/repo.git \
  --local-path /path/to/repo \
  --install-service
```

The config file is stored at:

```bash
~/.fixforge/runner.json
```

## Update

```bash
fixforge-client update
```

You can pin a version or repository:

```bash
fixforge-client update --version v0.1.0 --repo owner/fixforge-client
```

## Service

```bash
fixforge-client service install
fixforge-client service status
fixforge-client service stop
fixforge-client service start
fixforge-client service uninstall
```

Linux uses a user-level systemd service, macOS uses LaunchAgent, and Windows uses Windows Service.

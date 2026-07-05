package main

import (
	"fmt"
	"os"

	"github.com/HobbyBear/fixforge-client/internal/runner"
)

var (
	version           = "dev"
	defaultGitHubRepo = "HobbyBear/fixforge-client"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		if err := runner.DoRun(); err != nil {
			exitErr(err)
		}
	case "connect":
		if err := runner.DoConnect(os.Args[2:]); err != nil {
			exitErr(err)
		}
	case "service":
		if len(os.Args) < 3 {
			fmt.Fprintf(os.Stderr, "Usage: fixforge-client service install|uninstall|start|stop|status\n")
			os.Exit(1)
		}
		if err := runServiceCommand(os.Args[2]); err != nil {
			exitErr(err)
		}
	case "install":
		if err := runner.DoServiceInstall(); err != nil {
			exitErr(err)
		}
	case "update":
		if err := runner.DoUpdate(os.Args[2:], version, defaultGitHubRepo); err != nil {
			exitErr(err)
		}
	case "ps":
		if err := runner.DoPS(); err != nil {
			exitErr(err)
		}
	case "attach":
		if len(os.Args) < 3 {
			fmt.Fprintf(os.Stderr, "Usage: fixforge-client attach TASK\n")
			os.Exit(1)
		}
		if err := runner.DoAttach(os.Args[2]); err != nil {
			exitErr(err)
		}
	case "open":
		if len(os.Args) < 3 {
			fmt.Fprintf(os.Stderr, "Usage: fixforge-client open TASK\n")
			os.Exit(1)
		}
		if err := runner.DoOpen(os.Args[2]); err != nil {
			exitErr(err)
		}
	case "project":
		if len(os.Args) < 3 || os.Args[2] != "add" {
			fmt.Fprintf(os.Stderr, "Usage: fixforge-client project add\n")
			os.Exit(1)
		}
		if err := runner.DoAddProject(); err != nil {
			exitErr(err)
		}
	case "status":
		if err := runner.DoStatus(); err != nil {
			exitErr(err)
		}
	case "version", "--version", "-v":
		fmt.Printf("fixforge-client version %s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func runServiceCommand(command string) error {
	switch command {
	case "install":
		return runner.DoServiceInstall()
	case "uninstall", "remove":
		return runner.DoServiceUninstall()
	case "start":
		return runner.DoServiceStart()
	case "stop":
		return runner.DoServiceStop()
	case "status":
		return runner.DoServiceStatus()
	default:
		return fmt.Errorf("unknown service command: %s", command)
	}
}

func exitErr(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

func printUsage() {
	fmt.Println(`FixForge Client — 本地 AI 编码执行器

用法:
  fixforge-client connect --server URL --token TOKEN --project-id NAME --local-path PATH [--install-service]
  fixforge-client run                  启动本地 Client 守护进程
  fixforge-client service install      安装并启动系统服务
  fixforge-client service status       查看系统服务状态
  fixforge-client service stop         停止系统服务
  fixforge-client service uninstall    卸载系统服务
  fixforge-client update               从 GitHub Release 更新当前二进制
  fixforge-client ps                   查看本地 Run
  fixforge-client attach TASK          显示本地 Run 日志
  fixforge-client open TASK            显示本地 worktree 和分支
  fixforge-client project add          添加项目
  fixforge-client status               查看当前状态
  fixforge-client version              显示版本号

配置文件: ~/.fixforge/runner.json

快速开始:
  1. 在 FixForge 项目列表复制 Client 接入命令
  2. 在本地仓库根目录执行该命令
  3. 命令会安装 client、写入配置，并可通过 --install-service 常驻运行`)
}

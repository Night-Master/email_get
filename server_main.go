package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

type ServerOptions struct {
	ListenAddr   string
	DataDir      string
	Open         bool
	AuthPassword string
	FetchTimeout time.Duration
}

func main() {
	opts := parseServerOptions()

	app, err := NewWebApp(opts.DataDir)
	if err != nil {
		exitf("初始化应用失败: %v", err)
	}
	if opts.FetchTimeout > 0 {
		app.fetchTimeout = opts.FetchTimeout
	}
	if err := app.initAuth(opts.AuthPassword); err != nil {
		exitf("初始化认证失败: %v", err)
	}

	server := &http.Server{
		Addr:              opts.ListenAddr,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	url := fmt.Sprintf("http://%s", opts.ListenAddr)
	fmt.Printf("Web UI: %s\n", url)

	if opts.Open {
		go openBrowser(url)
	}

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		exitf("启动服务失败: %v", err)
	}
}

func parseServerOptions() ServerOptions {
	opts := ServerOptions{}
	defaultDataDir := defaultRuntimeDataDir()
	flag.StringVar(&opts.ListenAddr, "listen", "127.0.0.1:80", "Web 服务监听地址")
	flag.StringVar(&opts.DataDir, "data-dir", defaultDataDir, "应用数据目录")
	flag.StringVar(&opts.AuthPassword, "auth-password", "", "设置/重置管理密码(不填则沿用已有密码,首次启动自动生成)")
	flag.DurationVar(&opts.FetchTimeout, "fetch-timeout", 30*time.Second, "单条 IMAP/POP3 命令超时(如 30s、1m)")
	flag.BoolVar(&opts.Open, "open", false, "启动后自动打开浏览器")
	flag.Parse()
	return opts
}

func defaultRuntimeDataDir() string {
	exePath, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exePath)
}

func openBrowser(url string) {
	time.Sleep(600 * time.Millisecond)

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

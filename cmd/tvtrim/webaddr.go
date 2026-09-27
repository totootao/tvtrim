package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Web 监听地址的两种默认值。
const (
	// localWebAddr 是宿机上的默认监听地址:只监听回环,端口 0 由系统分配。
	// 不开放到公网,多人共用的机器上也不会被别人访问到。
	localWebAddr = "127.0.0.1:0"
	// containerWebAddr 是容器里的默认监听地址。
	// 容器内若按宿机的默认做法监听 127.0.0.1,宿机通过端口映射永远连不进来,
	// 因此容器里自动放开到 0.0.0.0,并用固定端口便于 -p 映射。
	containerWebAddr = "0.0.0.0:8080"
)

// 只给端口号时补上的默认主机(见 resolveWebAddr)。
const (
	localHostAddr     = "127.0.0.1"
	containerHostAddr = "0.0.0.0"
)

// externalURLHint 对监听在通配地址上的情况给出容器外的访问提示;
// 监听回环或地址解析失败时返回空串(不需要提示)。
func externalURLHint(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || (host != "0.0.0.0" && host != "::") {
		return ""
	}
	return fmt.Sprintf("容器端口 %s,用 docker -p <宿机端口>:%s 映射后访问 http://localhost:<宿机端口>/",
		port, port)
}

// envContainer 是手动开关容器行为的环境变量。
// 设为 1/true 强制按容器处理,0/false 强制按宿机处理(未设置时自动探测)。
const envContainer = "TVTRIM_CONTAINER"

// resolveWebAddr 决定 -web 的监听地址。
// addr 非空表示用户在命令行显式指定了 -addr,原样使用;
// 留空则按运行环境选择:容器内 0.0.0.0:8080,宿机 127.0.0.1:0。
func resolveWebAddr(addr string) string {
	if addr == "" {
		if inContainer() {
			return containerWebAddr
		}
		return localWebAddr
	}
	// 只给了端口号(如 -addr 8080):主机沿用当前环境的默认值,
	// 免得宿机上一次手滑就把界面暴露到局域网。
	if _, _, err := net.SplitHostPort(addr); err != nil {
		if _, convErr := strconv.Atoi(addr); convErr == nil {
			host := localHostAddr
			if inContainer() {
				host = containerHostAddr
			}
			return net.JoinHostPort(host, addr)
		}
	}
	return addr
}

// inContainer 判断当前进程是否运行在容器里。
func inContainer() bool {
	return looksContainer("/.dockerenv", "/proc/1/cgroup", os.Getenv)
}

// looksContainer 是 inContainer 的可测试版本:路径与环境变量读取都参数化。
//
// 判定顺序:
//  1. TVTRIM_CONTAINER 显式开关优先,便于强制某一种行为(也是测试入口);
//  2. /.dockerenv 存在(Docker 会在容器根目录放这个文件);
//  3. /proc/1/cgroup 里出现 docker / kubepods / containerd 等运行时标记。
func looksContainer(dockerEnvPath, cgroupPath string, getenv func(string) string) bool {
	if v := strings.ToLower(strings.TrimSpace(getenv(envContainer))); v != "" {
		switch v {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	if _, err := os.Stat(dockerEnvPath); err == nil {
		return true
	}
	b, err := os.ReadFile(cgroupPath)
	if err != nil {
		return false
	}
	s := string(b)
	for _, mark := range []string{"docker", "kubepods", "containerd", "crio", "lxc"} {
		if strings.Contains(s, mark) {
			return true
		}
	}
	return false
}

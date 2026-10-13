package reader

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Windows npm shims are shell programs. Resolve their installed native binary
// so prompt/model arguments never become shell code and cancellation owns it.
func agentExecutable(provider string) (string, error) {
	path, err := exec.LookPath(provider)
	if err != nil || runtime.GOOS != "windows" {
		return path, err
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".cmd" && ext != ".bat" {
		return path, nil
	}
	packageName := map[string]string{"codex": "@openai/codex", "claude": "@anthropic-ai/claude-code"}[provider]
	if packageName == "" {
		return "", errors.New("Agent 需要原生可执行程序")
	}
	prefix := filepath.Dir(path)
	root, err := filepath.EvalSymlinks(filepath.Join(prefix, "node_modules", filepath.FromSlash(packageName)))
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return "", err
	}
	var manifest struct {
		Name string            `json:"name"`
		Bin  map[string]string `json:"bin"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", err
	}
	if manifest.Name != packageName {
		return "", errors.New("Agent 安装包身份不符")
	}
	paths := []string{}
	if provider == "codex" {
		arch, triple := "x64", "x86_64-pc-windows-msvc"
		if runtime.GOARCH == "arm64" {
			arch, triple = "arm64", "aarch64-pc-windows-msvc"
		}
		if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
			return "", errors.New("Codex 不支持当前平台")
		}
		tail := filepath.Join("vendor", triple, "bin", "codex.exe")
		paths = append(paths,
			filepath.Join(root, "node_modules", "@openai", "codex-win32-"+arch, tail),
			filepath.Join(prefix, "node_modules", "@openai", "codex-win32-"+arch, tail),
			filepath.Join(root, tail))
	} else {
		bin := filepath.FromSlash(manifest.Bin[provider])
		if bin == "." || filepath.IsAbs(bin) || strings.ToLower(filepath.Ext(bin)) != ".exe" {
			return "", errors.New("Claude 安装包未提供原生程序")
		}
		native := filepath.Join(root, bin)
		rel, err := filepath.Rel(root, native)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", errors.New("Agent 程序路径无效")
		}
		paths = append(paths, native)
	}
	for _, native := range paths {
		if info, err := os.Stat(native); err == nil && !info.IsDir() {
			return native, nil
		}
	}
	return "", errors.New("Agent 原生程序缺失，请修复 CLI 安装")
}

func agentCommand(ctx context.Context, provider string, args ...string) *exec.Cmd {
	path, err := agentExecutable(provider)
	if err != nil {
		path = provider
	}
	var cmd *exec.Cmd
	if ctx == nil {
		cmd = exec.Command(path, args...)
	} else {
		cmd = exec.CommandContext(ctx, path, args...)
	}
	if err != nil {
		cmd.Err = err
	}
	return cmd
}

package reader

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Only parse the path preferences we need. prefs.js is never executed.
var zoteroPathPref = regexp.MustCompile(`^\s*user_pref\(\s*"extensions\.zotero\.(dataDir|useDataDir|baseAttachmentPath)"\s*,\s*(.*?)\s*\);\s*$`)

type zoteroPathPreferences struct {
	dataDir, linkedBase string
	useDataDir          bool
}

func readZoteroConfig(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("Zotero 配置不是普通文件")
	}
	const limit = 4 << 20
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) > limit {
		return nil, errors.New("Zotero 配置无法读取或过大")
	}
	return data, nil
}

func readZoteroPathPreferences(path string) (zoteroPathPreferences, error) {
	var prefs zoteroPathPreferences
	data, err := readZoteroConfig(path)
	if err != nil {
		return prefs, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		match := zoteroPathPref.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		switch match[1] {
		case "dataDir":
			err = json.Unmarshal([]byte(match[2]), &prefs.dataDir)
		case "useDataDir":
			err = json.Unmarshal([]byte(match[2]), &prefs.useDataDir)
		case "baseAttachmentPath":
			err = json.Unmarshal([]byte(match[2]), &prefs.linkedBase)
		}
		if err != nil {
			return prefs, errors.New("Zotero 路径配置格式无效")
		}
	}
	return prefs, nil
}

func zoteroProfileRoot(platform, userHome, appData string) string {
	switch platform {
	case "darwin":
		return filepath.Join(userHome, "Library", "Application Support", "Zotero")
	case "windows":
		if appData == "" {
			appData = filepath.Join(userHome, "AppData", "Roaming")
		}
		return filepath.Join(appData, "Zotero", "Zotero")
	default:
		return filepath.Join(userHome, ".zotero", "zotero")
	}
}

func zoteroProfileDirectories(root string) []string {
	data, err := readZoteroConfig(filepath.Join(root, "profiles.ini"))
	if err != nil {
		// Standard layouts also work when no profile registry is available.
		var profiles []string
		for _, pattern := range []string{"Profiles/*/prefs.js", "*/prefs.js"} {
			paths, _ := filepath.Glob(filepath.Join(root, pattern))
			for _, path := range paths {
				profiles = append(profiles, filepath.Dir(path))
			}
		}
		return profiles
	}
	var profiles []string
	var path, relative string
	profile := false
	appendProfile := func() {
		if !profile || path == "" {
			return
		}
		if relative == "1" && filepath.IsLocal(filepath.FromSlash(path)) {
			profiles = append(profiles, filepath.Join(root, filepath.FromSlash(path)))
		} else if relative == "0" && filepath.IsAbs(path) {
			profiles = append(profiles, path)
		}
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			appendProfile()
			profile = strings.HasPrefix(line, "[Profile")
			path, relative = "", ""
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !profile || !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Path":
			path = strings.TrimSpace(value)
		case "IsRelative":
			relative = strings.TrimSpace(value)
		}
	}
	if scanner.Err() != nil {
		return nil
	}
	appendProfile()
	return profiles
}

func zoteroLinkedBaseFromProfiles(directory, defaultDirectory string, profiles []string) (string, error) {
	selected, err := os.Stat(directory)
	if err != nil {
		return "", err
	}
	bases := map[string]bool{}
	for _, profile := range profiles {
		prefs, err := readZoteroPathPreferences(filepath.Join(profile, "prefs.js"))
		if err != nil {
			continue
		}
		dataDir := defaultDirectory
		if prefs.useDataDir && prefs.dataDir != "" {
			dataDir = prefs.dataDir
		}
		if !filepath.IsAbs(dataDir) {
			continue
		}
		info, err := os.Stat(dataDir)
		if err != nil || !os.SameFile(selected, info) {
			continue
		}
		base := prefs.linkedBase
		if filepath.IsAbs(base) {
			if resolved, err := filepath.EvalSymlinks(base); err == nil {
				base = resolved
			} else {
				base = filepath.Clean(base)
			}
		}
		bases[base] = true
	}
	if len(bases) > 1 {
		return "", errors.New("多个 Zotero 配置对该资料库设置了不同的链接附件基目录，请手动填写")
	}
	for base := range bases {
		if base == "" {
			break
		}
		info, err := os.Stat(base)
		if !filepath.IsAbs(base) || err != nil || !info.IsDir() {
			return "", errors.New("Zotero 配置中的链接附件基目录不存在或无法访问，请手动填写本机目录")
		}
		return base, nil
	}
	return "", errors.New("未能从本机 Zotero 配置识别此资料库的链接附件基目录，请在“使用链接附件”中手动填写")
}

func discoverZoteroLinkedBase(directory string) (string, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("无法定位 Zotero 配置，请手动填写链接附件基目录")
	}
	root := zoteroProfileRoot(runtime.GOOS, userHome, os.Getenv("APPDATA"))
	return zoteroLinkedBaseFromProfiles(directory, filepath.Join(userHome, "Zotero"), zoteroProfileDirectories(root))
}

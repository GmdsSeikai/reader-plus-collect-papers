package reader

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestPaperAnalysisWindowsNPMNativeLaunch(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows npm command shim boundary")
	}
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			fakeAgent(t, provider, "analysis-session")
			binary, err := exec.LookPath(provider)
			if err != nil {
				t.Fatal(err)
			}
			prefix := filepath.Dir(binary)
			t.Setenv("READER_ANALYSIS_CAPTURE", filepath.Join(prefix, "native-analysis.jsonl"))
			packageName := "@anthropic-ai/claude-code"
			bin := "bin/claude.exe"
			if provider == "codex" {
				packageName, bin = "@openai/codex", "bin/codex.js"
			}
			root := filepath.Join(prefix, "node_modules", filepath.FromSlash(packageName))
			native := filepath.Join(root, "bin", "claude.exe")
			if provider == "codex" {
				arch, triple := "x64", "x86_64-pc-windows-msvc"
				if runtime.GOARCH == "arm64" {
					arch, triple = "arm64", "aarch64-pc-windows-msvc"
				}
				native = filepath.Join(root, "node_modules", "@openai", "codex-win32-"+arch, "vendor", triple, "bin", "codex.exe")
			}
			if err := os.MkdirAll(filepath.Dir(native), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(binary, native); err != nil {
				t.Fatal(err)
			}
			manifest := map[string]any{"name": packageName, "bin": map[string]string{provider: bin}}
			if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(mustJSONText(manifest)), 0600); err != nil {
				t.Fatal(err)
			}
			// A shell fallback would fail; the known package must launch directly.
			if err := os.WriteFile(filepath.Join(prefix, provider+".cmd"), []byte("@exit /b 98\r\n"), 0600); err != nil {
				t.Fatal(err)
			}
			s := testServer(t)
			candidate := seedAnalysisCandidate(t, s)
			ai := AIConfig{Primary: provider, Models: map[string]string{}, Capabilities: map[string]Capability{}}
			ai.Capabilities[provider] = Capability{Text: true, CheckedAt: "fixture", Fingerprint: configPrint(ai, provider)}
			if err := s.writeAIConfig(ai); err != nil {
				t.Fatal(err)
			}
			cfg := defaultCollectionSettings()
			cfg.AnalysisEnabled, cfg.AnalysisProvider, cfg.AnalysisModel = true, provider, "test-model"
			if err := s.Store.SaveCollectionSettings(cfg); err != nil {
				t.Fatal(err)
			}
			job := submitAnalysisHTTP(t, s, candidate.ArXivID)
			if job := waitAnalysisJobHTTP(t, s, job.ID); job.Status != runSucceeded {
				t.Fatalf("native npm launcher: %+v", job)
			}
			var sent int
			if err := s.Store.DB.QueryRow("SELECT sent FROM collection_quota_days").Scan(&sent); err != nil || sent != 1 {
				t.Fatalf("native dispatch accounting: %d %v", sent, err)
			}
		})
	}
}

func TestInstalledAgentNativeVersion(t *testing.T) {
	if os.Getenv("READER_TEST_NATIVE_LAUNCH") != "1" {
		t.Skip("installed CLI version evidence is explicitly selected")
	}
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := agentCommand(ctx, provider, "--version")
			output, err := cmd.Output()
			if err != nil || len(output) == 0 {
				t.Fatalf("installed native --version: %v", err)
			}
			t.Logf("%s: %s", cmd.Path, output)
		})
	}
}

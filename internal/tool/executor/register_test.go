package executor

import (
	"slices"
	"testing"

	"github.com/lush/blowball/internal/config"
)

func TestBuildPipCommand(t *testing.T) {
	cfg := config.PipToolConfig{
		IndexURL:       "https://pypi.tuna.tsinghua.edu.cn/simple",
		ExtraIndexURLs: []string{"https://extra.example.com/simple"},
		TrustedHosts:   []string{"pypi.tuna.tsinghua.edu.cn", "extra.example.com"},
	}
	args := pipArgs{Packages: []string{"requests", "numpy>=2.0"}, Upgrade: true}

	cmd, err := buildPipCommand(cfg, args)
	if err != nil {
		t.Fatalf("buildPipCommand returned error: %v", err)
	}

	want := []string{
		"python3", "-m", "pip", "install", "--target", "/workspace/.pip",
		"--upgrade",
		"-i", "https://pypi.tuna.tsinghua.edu.cn/simple",
		"--extra-index-url", "https://extra.example.com/simple",
		"--trusted-host", "pypi.tuna.tsinghua.edu.cn",
		"--trusted-host", "extra.example.com",
		"requests", "numpy>=2.0",
	}
	if len(cmd) != len(want) {
		t.Fatalf("command length mismatch: got %d, want %d\ngot: %v", len(cmd), len(want), cmd)
	}
	for i := range want {
		if cmd[i] != want[i] {
			t.Errorf("cmd[%d] = %q, want %q", i, cmd[i], want[i])
		}
	}
}

func TestBuildPipCommandMinimal(t *testing.T) {
	cfg := config.PipToolConfig{}
	args := pipArgs{Packages: []string{"colorama"}}

	cmd, err := buildPipCommand(cfg, args)
	if err != nil {
		t.Fatalf("buildPipCommand returned error: %v", err)
	}

	want := []string{"python3", "-m", "pip", "install", "--target", "/workspace/.pip", "colorama"}
	if len(cmd) != len(want) {
		t.Fatalf("command length mismatch: got %d, want %d\ngot: %v", len(cmd), len(want), cmd)
	}
	for i := range want {
		if cmd[i] != want[i] {
			t.Errorf("cmd[%d] = %q, want %q", i, cmd[i], want[i])
		}
	}
}

func TestBuildPipCommandIgnoresEmptyOptions(t *testing.T) {
	cfg := config.PipToolConfig{
		IndexURL:       "",
		ExtraIndexURLs: []string{"", "https://valid.example.com/simple", ""},
		TrustedHosts:   []string{"", "valid.example.com", ""},
	}
	args := pipArgs{Packages: []string{"requests"}}

	cmd, err := buildPipCommand(cfg, args)
	if err != nil {
		t.Fatalf("buildPipCommand returned error: %v", err)
	}

	for i, v := range cmd {
		if v == "" {
			t.Errorf("command contains empty argument at index %d: %v", i, cmd)
		}
	}
	if slices.Contains(cmd, "-i") {
		t.Error("expected no -i flag for empty index_url")
	}
}

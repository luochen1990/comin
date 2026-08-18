package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 验证 YAML null 与缺省字段均解码为 Go 零值 "" (策略不误触发),
// 以及 build_confirmer.reboot_policy 非空被拒绝 (策略仅适用于 deploy 侧).
func TestRebootPolicyConfigDecoding(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")

	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"explicit null", "repository_type: flake\ndeploy_confirmer:\n  mode: auto\n  reboot_policy: null\n", ""},
		{"omitted", "repository_type: flake\ndeploy_confirmer:\n  mode: auto\n", ""},
		{"set", "repository_type: flake\ndeploy_confirmer:\n  mode: auto\n  reboot_policy: skip\n", "skip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(p, []byte(tc.yaml), 0644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Read(p)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.DeployConfirmer.RebootPolicy != tc.want {
				t.Fatalf("want %q, got %q", tc.want, cfg.DeployConfirmer.RebootPolicy)
			}
		})
	}

	// build 侧配了策略: 必须报错 (fail fast, 不静默吞配置)
	os.WriteFile(p, []byte("repository_type: flake\nbuild_confirmer:\n  mode: auto\n  reboot_policy: skip\n"), 0644)
	if _, err := Read(p); err == nil {
		t.Fatal("want error for build_confirmer.reboot_policy, got nil")
	}
}

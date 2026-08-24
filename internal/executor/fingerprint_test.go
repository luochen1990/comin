package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// mkFingerprintCache 写一个临时指纹缓存文件, 返回其路径.
func mkFingerprintCache(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "check-cache.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestLookupFingerprint(t *testing.T) {
	// "存在于 store" 的判定只依赖 os.Stat — 用临时文件/不存在的路径
	// 分别充当命中与未命中, 不依赖 /nix/store 的存在 (CI 可移植).
	present := filepath.Join(t.TempDir(), "fake-store-path")
	require.NoError(t, os.WriteFile(present, nil, 0o644))
	absent := filepath.Join(t.TempDir(), "fingerprint-absent-test")

	cache := mkFingerprintCache(t, `{
		"treehash1": {"host-a": {"drvPath": "/nix/store/d1.drv", "outPath": "`+present+`"}},
		"treehash2": {"host-a": {"drvPath": "/nix/store/d2.drv", "outPath": "`+absent+`"}},
		"treehash3": {"host-b": {"drvPath": "", "outPath": "`+present+`"}}
	}`)

	tests := []struct {
		name        string
		cachePath   string
		treeHash    string
		hostname    string
		wantOk      bool
		wantDrvPath string
		wantOutPath string
	}{
		{"hit present outPath", cache, "treehash1", "host-a", true, "/nix/store/d1.drv", present},
		{"hit but outPath not in store (eval-only entry)", cache, "treehash2", "host-a", false, "", ""},
		{"tree known but wrong host", cache, "treehash1", "host-b", false, "", ""},
		{"hit with empty drvPath still usable (already-built path)", cache, "treehash3", "host-b", true, "", present},
		{"unknown tree", cache, "nope", "host-a", false, "", ""},
		{"empty cachePath disables feature", "", "treehash1", "host-a", false, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drvPath, outPath, ok := lookupFingerprint(tt.cachePath, tt.treeHash, tt.hostname)
			require.Equal(t, tt.wantOk, ok)
			require.Equal(t, tt.wantDrvPath, drvPath)
			require.Equal(t, tt.wantOutPath, outPath)
		})
	}
}

func TestLookupFingerprintMalformed(t *testing.T) {
	bad := mkFingerprintCache(t, `{not json`)
	_, _, ok := lookupFingerprint(bad, "treehash1", "host-a")
	require.False(t, ok)

	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	_, _, ok = lookupFingerprint(missing, "treehash1", "host-a")
	require.False(t, ok)
}

func TestCommitTreeHash(t *testing.T) {
	// 建一个含单 commit 的仓库, 验证 commitTreeHash 返回其 tree hash.
	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := r.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "flake.nix"), []byte("{}\n"), 0o644))
	_, err = wt.Add("flake.nix")
	require.NoError(t, err)
	h, err := wt.Commit("test", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t"},
	})
	require.NoError(t, err)
	commit, err := r.CommitObject(h)
	require.NoError(t, err)

	got, err := commitTreeHash(dir, h.String())
	require.NoError(t, err)
	require.Equal(t, commit.TreeHash.String(), got)

	// bare 仓库同样可读 (comin 的实际仓库形态).
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	_, err = git.PlainClone(bareDir, true, &git.CloneOptions{URL: dir})
	require.NoError(t, err)
	got, err = commitTreeHash(bareDir, h.String())
	require.NoError(t, err)
	require.Equal(t, commit.TreeHash.String(), got)

	// 未知 commit → error (调用方按 miss 处理).
	_, err = commitTreeHash(bareDir, "0000000000000000000000000000000000000000")
	require.Error(t, err)
}

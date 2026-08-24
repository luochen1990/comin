// 指纹缓存消费端 (只读).
//
// 背景: luochen1990/nixos 仓库的 scripts/build 在开发机上维护一个
// "工作区 tree hash → {host → {drvPath, outPath}}" 的指纹缓存 (跳过重复
// nix 求值的命令级开关). 本文件让 comin 在 eval 前查询该缓存: 若部署
// commit 的 tree 已被本机构建过 (outPath 仍在 store), 则直接复用求值
// 结果, 跳过两次 nix 求值 (~72s → 0s).
//
// 契约 (SSOT: scripts/build 头注释 "指纹缓存" 章节, 两端同步维护):
//
//	文件: JSON, {treeHash: {hostname: {drvPath: str, outPath: str, time: num}}}
//	键:   treeHash = git commit^{tree} (clean 工作树构建时即 HEAD^{tree},
//	      与 comin 从 commitId 推导的 TreeHash 天然一致)
//	值:   drvPath 可能为 "" (仅 build 未 eval 的条目); outPath 恒有效.
//
// 安全性: outPath 是内容寻址的 store path, "存在于 store" 即可信;
// 指纹错误的最坏结果是 miss (回退正常求值). 假阳性需要 SHA-1 碰撞,
// 可忽略. 注意: 命中时跳过 eval 也就跳过了 services.comin.machineId
// 校验 — 等价保护来自缓存按 hostname (nixosConfigurations attr 名) 键控.
package executor

import (
	"encoding/json"
	"os"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/sirupsen/logrus"
)

// fingerprintHostEntry 是缓存中单 host 条目的结构 (time 字段被消费端忽略).
type fingerprintHostEntry struct {
	DrvPath string `json:"drvPath"`
	OutPath string `json:"outPath"`
}

// commitTreeHash 返回 commit 的 tree hash (指纹缓存的键).
// repositoryPath 是 comin 的本地 bare 仓库 (go-git PlainOpen 兼容 bare).
func commitTreeHash(repositoryPath, commitId string) (string, error) {
	r, err := git.PlainOpen(repositoryPath)
	if err != nil {
		return "", err
	}
	commit, err := r.CommitObject(plumbing.NewHash(commitId))
	if err != nil {
		return "", err
	}
	return commit.TreeHash.String(), nil
}

// lookupFingerprint 查询指纹缓存.
// 命中条件: cachePath 可读且合法 JSON, 且 cache[treeHash][hostname] 的
// outPath 存在于本地 store (这才是可跳过 eval 的充分条件 — 仅求值未
// 构建的条目对 comin 无用, 仍需正常 eval 拿 drvPath 去 build).
// 任何失败 (文件缺失/损坏/未命中/store 无此路径) 一律返回 miss, 回退
// 正常求值路径.
func lookupFingerprint(cachePath, treeHash, hostname string) (drvPath, outPath string, ok bool) {
	if cachePath == "" {
		return "", "", false
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		logrus.Debugf("fingerprint: cache %s unreadable: %s", cachePath, err)
		return "", "", false
	}
	var cache map[string]map[string]fingerprintHostEntry
	if err := json.Unmarshal(data, &cache); err != nil {
		logrus.Debugf("fingerprint: cache %s malformed: %s", cachePath, err)
		return "", "", false
	}
	entry, hit := cache[treeHash][hostname]
	if !hit || entry.OutPath == "" || !isStorePathExist(entry.OutPath) {
		return "", "", false
	}
	return entry.DrvPath, entry.OutPath, true
}

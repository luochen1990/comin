// 职责边界: desktop 通知状态机的单元测试 (不依赖 dbus/notifier).
// notifier 为 nil 时 showOrUpdateLocked 是 no-op, 可直接驱动 handler 断言内部状态.
package cmd

import (
	"testing"

	"github.com/nlewo/comin/internal/builder"
	"github.com/nlewo/comin/pkg/protobuf"
)

// 复现 (Bug: 通知展示旧 commit msg):
// 指纹缓存命中的 generation 走 already-built 快路径 (builder.Eval 内
// IsStorePathExist 命中直接 GenerationBuildStart(AlreadyBuilt)), 其
// BuildStarted 事件在 handler 中被 BuildReason gate 跳过 — commit header
// 若仅在 gate 之后的 need-build 分支刷新, 会跨周期残留上一个周期的旧 msg.
func TestBuildStartedRefreshesCommitHeaderOnAlreadyBuilt(t *testing.T) {
	state := &notificationState{pendingChecks: &protobuf.RebootChecks{}}

	buildStarted := func(reason, commitId, msg string) {
		g := &protobuf.Generation{
			BuildReason: reason,
			Source: &protobuf.Source{
				Source: &protobuf.Source_Git{
					Git: &protobuf.Git{
						SelectedCommitId:  commitId,
						SelectedCommitMsg: msg,
					},
				},
			},
		}
		handler(&protobuf.Event{
			Type: &protobuf.Event_BuildStartedType{BuildStartedType: &protobuf.Event_BuildStarted{Generation: g}},
		}, state, nil)
	}

	// 周期 1: need-build 路径, header 写入 commit A
	buildStarted(builder.BuildReasonNeedBuild,
		"aaaaaaaa111111111111111111111111111111111", "feat(old): previous cycle\n\nbody")
	if state.commitSubject != "feat(old): previous cycle" {
		t.Fatalf("cycle 1: commitSubject = %q, want %q", state.commitSubject, "feat(old): previous cycle")
	}

	// 周期 2: already-built 快路径 (指纹缓存命中), header 必须刷新为 commit B
	buildStarted(builder.BuildReasonAlreadyBuilt,
		"bbbbbbbb222222222222222222222222222222222", "feat(new): fingerprint cache hit")
	if state.commitSubject != "feat(new): fingerprint cache hit" {
		t.Fatalf("cycle 2 (already built): commitSubject = %q, want %q — 旧周期 commit header 残留",
			state.commitSubject, "feat(new): fingerprint cache hit")
	}
	if state.commitShortID != "bbbbbbbb" {
		t.Fatalf("cycle 2: commitShortID = %q, want %q", state.commitShortID, "bbbbbbbb")
	}
	// 锚定 gate 语义: already-built 不进入 need-build 分支 (不开"正在构建"通知,
	// cycle 不递增) — 防止未来误删 gate 导致 already-built 也走完整通知生命周期.
	if state.cycle != 1 {
		t.Fatalf("cycle 2 (already built): cycle = %d, want 1 — already-built 不应递增 cycle", state.cycle)
	}
}

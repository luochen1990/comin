// 职责边界: generation 事件流快照语义的单元测试.
// 事件发布给 broker 后会被 server goroutine 异步 marshal — 发布的必须是
// 发布时刻的快照, 不得与 store 内部的可变 generation 对象共享指针.
package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/pkg/protobuf"
	"github.com/stretchr/testify/assert"
)

// receiveEvents 从订阅通道收满 n 条事件, 超时 fail.
func receiveEvents(t *testing.T, sub chan *protobuf.Event, n int) []*protobuf.Event {
	t.Helper()
	events := make([]*protobuf.Event, 0, n)
	for i := 0; i < n; i++ {
		select {
		case ev := <-sub:
			events = append(events, ev)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for event %d/%d", i+1, n)
		}
	}
	return events
}

// 复现 (Bug: gRPC marshal "size mismatch"):
// already-built 快路径下 EvalFinished/BuildStart/BuildFinished 背靠背修改
// 同一 generation 对象. 若事件携带的是活指针, server goroutine 异步 marshal
// 撞上后续修改即产生 size mismatch / 撕裂读 (journal 2026-08-24 15:14:40
// 实录). 断言: 已发布事件冻结在发布时刻, 不随后续状态变更漂移.
func TestGenerationEventsAreSnapshots(t *testing.T) {
	b := broker.New()
	b.Start()
	t.Cleanup(b.Stop)
	sub := b.Subscribe()

	dir := t.TempDir()
	s, err := New(b, filepath.Join(dir, "store.json"), dir, 3, 3, 5)
	assert.NoError(t, err)

	rs := &protobuf.GitRepositoryStatus{
		SelectedRemoteName: "origin",
		SelectedBranchName: "master",
		SelectedCommitId:   "0123456789abcdef0123456789abcdef01234567",
		SelectedCommitMsg:  "feat(test): snapshot semantics",
	}
	g := s.NewGeneration("host", "", "system", rs)

	assert.NoError(t, s.GenerationEvalStarted(g.Uuid))
	assert.NoError(t, s.GenerationEvalFinished(g.Uuid, "/nix/store/drv", "/nix/store/out", "", nil))
	assert.NoError(t, s.GenerationBuildStart(g.Uuid, "already built"))
	assert.NoError(t, s.GenerationBuildFinished(g.Uuid, nil))

	events := receiveEvents(t, sub, 4)

	evalStarted := events[0].GetEvalStartedType().GetGeneration()
	assert.Equal(t, "evaluating", evalStarted.EvalStatus,
		"EvalStarted 事件应冻结在 evaluating, 不随后续 EvalFinished 漂移")
	assert.Empty(t, evalStarted.OutPath,
		"EvalStarted 事件不应看到后续 EvalFinished 才写入的 OutPath")

	buildStarted := events[2].GetBuildStartedType().GetGeneration()
	assert.Equal(t, "building", buildStarted.BuildStatus,
		"BuildStarted 事件应冻结在 building, 不随后续 BuildFinished 漂移")
	assert.Equal(t, "already built", buildStarted.BuildReason)

	buildFinished := events[3].GetBuildFinishedType().GetGeneration()
	assert.Equal(t, "built", buildFinished.BuildStatus)
}

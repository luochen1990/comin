package broker

import (
	"bufio"
	"io"
	"sync"
	"time"

	"github.com/nlewo/comin/pkg/protobuf"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Broker struct {
	stopCh      chan struct{}
	publishCh   chan *protobuf.Event
	subscribers map[chan *protobuf.Event]struct{}
	mu          sync.RWMutex
}

func New() *Broker {
	return &Broker{
		stopCh:      make(chan struct{}),
		publishCh:   make(chan *protobuf.Event, 1),
		subscribers: make(map[chan *protobuf.Event]struct{}),
	}
}

func (b *Broker) Start() {
	go func() {
		for {
			select {
			case <-b.stopCh:
				return
			case msg := <-b.publishCh:
				b.mu.RLock()
				for msgCh := range b.subscribers {
					// msgCh is buffered, use non-blocking send to protect the broker:
					select {
					case msgCh <- msg:
					default:
					}
				}
				b.mu.RUnlock()
			}
		}
	}()
}

func (b *Broker) Stop() {
	close(b.stopCh)
}

func (b *Broker) Subscribe() chan *protobuf.Event {
	msgCh := make(chan *protobuf.Event, 5)
	b.mu.Lock()
	b.subscribers[msgCh] = struct{}{}
	b.mu.Unlock()
	return msgCh
}

func (b *Broker) Unsubscribe(msgCh chan *protobuf.Event) {
	b.mu.Lock()
	delete(b.subscribers, msgCh)
	b.mu.Unlock()
}

// Publish 发布一条事件到所有订阅者.
//
// 快照契约 (SSOT): msg 的载荷必须是发布时刻的快照 (发布后不可变) — 不得与任何
// 会被继续修改的对象共享指针. 消费方 (gRPC server) 会跨 goroutine 异步 marshal,
// 活指针在 marshal 撞上并发修改时产生 "size mismatch" / 撕裂读并断开事件流.
// 发布方负责在 Publish 前 proto.CloneOf 活载荷.
func (b *Broker) Publish(msg *protobuf.Event) {
	b.publishCh <- msg
}

// GetLogger return a stdout et stderr writers. They have to be closed by the caller.
func (b *Broker) GetLogger(obj, objUuid string) (stdout, stderr io.WriteCloser) {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	b.Publish(
		&protobuf.Event{
			Type: &protobuf.Event_Log_{
				Log: &protobuf.Event_Log{
					ObjectType: obj,
					ObjectUuid: objUuid,
					Type: &protobuf.Event_Log_Open_{
						Open: &protobuf.Event_Log_Open{},
					},
				},
			},
			CreatedAt: timestamppb.New(time.Now().UTC()),
		},
	)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		logrus.Debug("broker: starting to scan stdout")
		scanner := bufio.NewScanner(stdoutR)
		for scanner.Scan() {
			line := scanner.Text()
			logrus.Infof("logs: %s", line)
			b.Publish(
				&protobuf.Event{
					Type: &protobuf.Event_Log_{
						Log: &protobuf.Event_Log{
							ObjectType: obj,
							ObjectUuid: objUuid,
							Type: &protobuf.Event_Log_Line_{
								Line: &protobuf.Event_Log_Line{
									Source: "stdout",
									Msg:    line,
								},
							},
						},
					},
					CreatedAt: timestamppb.New(time.Now().UTC()),
				},
			)
		}
		logrus.Debug("broken: stdout/" + obj + "/" + objUuid + ": stdout closed")
	}()

	go func() {
		defer wg.Done()
		logrus.Debug("broker: starting to scan stderr")
		scanner := bufio.NewScanner(stderrR)
		for scanner.Scan() {
			line := scanner.Text()
			logrus.Infof("logs: %s", line)
			b.Publish(
				&protobuf.Event{
					Type: &protobuf.Event_Log_{
						Log: &protobuf.Event_Log{
							ObjectType: obj,
							ObjectUuid: objUuid,
							Type: &protobuf.Event_Log_Line_{
								Line: &protobuf.Event_Log_Line{
									Source: "stderr",
									Msg:    line,
								},
							},
						},
					},
					CreatedAt: timestamppb.New(time.Now().UTC()),
				},
			)
		}
		logrus.Debug("broken: stdout/" + obj + "/" + objUuid + ": stdout closed")
	}()

	go func() {
		wg.Wait()
		b.Publish(
			&protobuf.Event{
				Type: &protobuf.Event_Log_{
					Log: &protobuf.Event_Log{
						ObjectType: obj,
						ObjectUuid: objUuid,
						Type: &protobuf.Event_Log_Close_{
							Close: &protobuf.Event_Log_Close{},
						},
					},
				},
				CreatedAt: timestamppb.New(time.Now().UTC()),
			},
		)
	}()

	return stdoutW, stderrW
}

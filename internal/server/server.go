package server

import (
	"context"
	"log"
	"net"
	"os"
	"time"

	"github.com/nlewo/comin/internal/broker"
	"github.com/nlewo/comin/internal/manager"
	"github.com/nlewo/comin/pkg/protobuf"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"google.golang.org/grpc"
)

type cominServer struct {
	protobuf.CominServer
	manager        *manager.Manager
	broker         *broker.Broker
	unixSocketPath string
}

func (s *cominServer) Events(_ *emptypb.Empty, stream grpc.ServerStreamingServer[protobuf.Event]) error {
	logrus.Infof("server: start to stream events")

	subscriber := s.broker.Subscribe()
	state := s.manager.GetState()
	stateEvent := &protobuf.Event{Type: &protobuf.Event_ManagerState_{ManagerState: &protobuf.Event_ManagerState{State: state}}, CreatedAt: timestamppb.New(time.Now().UTC())}
	if err := stream.Send(stateEvent); err != nil {
		logrus.Infof("server: failed to send stream: %s", err)
		s.broker.Unsubscribe(subscriber)
		return err
	}

	for {

		event := <-subscriber
		if err := stream.Send(event); err != nil {
			logrus.Infof("server: failed to send stream: %s", err)
			s.broker.Unsubscribe(subscriber)
			return err
		}
	}
}

func (s *cominServer) GetState(ctx context.Context, empty *emptypb.Empty) (*protobuf.State, error) {
	return s.manager.GetState(), nil
}

func (s *cominServer) Fetch(ctx context.Context, empty *emptypb.Empty) (*emptypb.Empty, error) {
	fetcher := s.manager.GetState().Fetcher
	gitRepoStatus := fetcher.GetGitRepositoryStatus()
	if gitRepoStatus == nil {
		return nil, nil
	}
	remotes := make([]string, 0)
	for _, r := range gitRepoStatus.Remotes {
		remotes = append(remotes, r.Name)
	}
	s.manager.Fetcher.TriggerFetch(remotes)
	return nil, nil
}

func (s *cominServer) DeploymentLatestSubmit(ctx context.Context, operation *protobuf.Operation) (*emptypb.Empty, error) {
	err := s.manager.DeploymentLatestSubmit(operation.OperationSubmitted)
	if err != nil {
		st := status.New(codes.Aborted, err.Error())
		err = st.Err()
	}
	return nil, err
}

func (s *cominServer) Suspend(ctx context.Context, empty *emptypb.Empty) (*emptypb.Empty, error) {
	err := s.manager.Suspend()
	if err != nil {
		st := status.New(codes.Aborted, err.Error())
		err = st.Err()
	}
	return nil, err
}
func (s *cominServer) Resume(ctx context.Context, empty *emptypb.Empty) (*emptypb.Empty, error) {
	err := s.manager.Resume(ctx)
	if err != nil {
		st := status.New(codes.Aborted, err.Error())
		err = st.Err()
	}
	return nil, err
}

func (s *cominServer) Confirm(ctx context.Context, req *protobuf.ConfirmRequest) (*emptypb.Empty, error) {
	switch req.For {
	case "build":
		s.manager.BuildConfirmer.Confirm(req.GenerationUuid)
	case "deploy":
		s.manager.DeployConfirmer.Confirm(req.GenerationUuid)
	case "all":
		s.manager.BuildConfirmer.Confirm(req.GenerationUuid)
		s.manager.DeployConfirmer.Confirm(req.GenerationUuid)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "invalid 'for' value: %q (want build/deploy/all)", req.For)
	}
	return nil, nil
}

// Cancel 取消处于 submitted 状态的 confirmation. 复用 Confirmer.Cancel() 现有能力, 不动核心逻辑.
// req.GenerationUuid 仅用于日志, 服务端不做匹配校验 (若与当前 submitted 不符则 Cancel 是 no-op, 无害).
func (s *cominServer) Cancel(ctx context.Context, req *protobuf.CancelRequest) (*emptypb.Empty, error) {
	switch req.For {
	case "build":
		s.manager.BuildConfirmer.Cancel()
	case "deploy":
		s.manager.DeployConfirmer.Cancel()
	case "all":
		s.manager.BuildConfirmer.Cancel()
		s.manager.DeployConfirmer.Cancel()
	default:
		return nil, status.Errorf(codes.InvalidArgument, "invalid 'for' value: %q (want build/deploy/all)", req.For)
	}
	return nil, nil
}

// Reboot 触发系统 reboot. 由 desktop 客户端的 reboot 交互通知 ("立即重启" 按钮) 调用.
// manager 以 root 运行, 直接执行 systemctl reboot.
// 不检查 rebootStatus: 信任客户端决策 (用户可能基于 pendingChecks 而非累积 rebootStatus 决定 reboot).
func (s *cominServer) Reboot(ctx context.Context, empty *emptypb.Empty) (*emptypb.Empty, error) {
	s.manager.RequestReboot()
	return nil, nil
}

func (c *cominServer) Start() {
	go func() {
		if _, err := os.Stat(c.unixSocketPath); err == nil {
			conn, err := net.Dial("unix", c.unixSocketPath)
			if err == nil {
				_ = conn.Close()
				logrus.Fatalf("server: socket %s already in use by another server", c.unixSocketPath)
			}
		}
		if err := os.RemoveAll(c.unixSocketPath); err != nil {
			log.Fatalf("server: failed to remove existing socket file: %s", err)
		}
		logrus.Infof("server: GRPC server starts listening on the Unix socket %s", c.unixSocketPath)
		lis, err := net.Listen("unix", c.unixSocketPath)
		if err != nil {
			log.Fatalf("server: failed to listen on %s: %s", c.unixSocketPath, err)
		}
		if err := os.Chmod(c.unixSocketPath, 0777); err != nil {
			log.Fatalf("server: failed to change socket permissions: %s", err)
		}
		var opts []grpc.ServerOption
		grpcServer := grpc.NewServer(opts...)
		protobuf.RegisterCominServer(grpcServer, c)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("server: failed to serve: %s", err)
		}
	}()
}

func New(broker *broker.Broker, manager *manager.Manager, unixSocketPath string) *cominServer {
	return &cominServer{
		manager:        manager,
		unixSocketPath: unixSocketPath,
		broker:         broker,
	}
}

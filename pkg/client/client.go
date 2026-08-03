package client

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/nlewo/comin/pkg/protobuf"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

type Client struct {
	conn        *grpc.ClientConn
	cominClient protobuf.CominClient
}

type ClientOpts struct {
	UnixSocketPath string
}

func New(clientOpts ClientOpts) (c Client, err error) {
	serverAddr := fmt.Sprintf("unix://%s", clientOpts.UnixSocketPath)
	logrus.Debugf("client: connection to %s", serverAddr)
	var opts []grpc.DialOption
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	c.conn, err = grpc.NewClient(serverAddr, opts...)
	if err != nil {
		return
	}
	c.cominClient = protobuf.NewCominClient(c.conn)
	return
}
func (c Client) Close() {
	c.conn.Close() // nolint: errcheck
}

func (c Client) GetManagerState() (state *protobuf.State, err error) {
	return c.cominClient.GetState(context.Background(), &emptypb.Empty{})
}

type Streamer struct {
	FailureMsg string
	Event      *protobuf.Event
}

func (c Client) Stream(ctx context.Context) (ch chan Streamer) {
	ch = make(chan Streamer)
	go func() {
		for {
			events, err := c.cominClient.Events(ctx, &emptypb.Empty{})
			if err != nil {
				reason := fmt.Sprintf("failed to connect to the stream: %s", err)
				logrus.Debug(reason)
				ch <- Streamer{FailureMsg: reason}
				time.Sleep(time.Second)
				continue
			}
			for {
				event, err := events.Recv()
				if err == io.EOF {
					reason := fmt.Sprintf("server closed stream: %s", err)
					logrus.Debug(reason)
					ch <- Streamer{FailureMsg: reason}
					break
				}
				if err != nil {
					reason := fmt.Sprintf("failed to receive from the stream: %s", err)
					logrus.Debug(reason)
					ch <- Streamer{FailureMsg: reason}
					break
				}
				ch <- Streamer{Event: event}
			}
		}
	}()
	return ch
}

func (c Client) Fetch() {
	c.cominClient.Fetch(context.Background(), &emptypb.Empty{}) // nolint: errcheck
}
func (c Client) Suspend() error {
	_, err := c.cominClient.Suspend(context.Background(), &emptypb.Empty{})
	return err
}
func (c Client) Resume() error {
	_, err := c.cominClient.Resume(context.Background(), &emptypb.Empty{})
	return err
}
func (c Client) DeploymentLatestSubmit(operation string) error {
	_, err := c.cominClient.DeploymentLatestSubmit(context.Background(), &protobuf.Operation{OperationSubmitted: operation})
	return err
}

func (c Client) Confirm(generationUUID, for_ string) error {
	_, err := c.cominClient.Confirm(context.Background(), &protobuf.ConfirmRequest{
		GenerationUuid: generationUUID, For: for_})
	return err
}

// Cancel 取消处于 submitted 状态的 confirmation. for_ 语义同 Confirm: "build"/"deploy"/"all".
// generationUUID 仅用于服务端日志, 不做匹配校验 (传空串亦可).
func (c Client) Cancel(generationUUID, for_ string) error {
	_, err := c.cominClient.Cancel(context.Background(), &protobuf.CancelRequest{
		GenerationUuid: generationUUID, For: for_})
	return err
}

// Reboot 请求主进程执行 systemctl reboot. 由 desktop 客户端的 reboot 交互通知 ("立即重启" 按钮) 调用.
// 主进程以 root 运行, 有权限执行 reboot; 客户端 (systemd user service) 无 root 权限, 故走 RPC.
// 不检查 rebootStatus: 信任客户端决策 (用户可能基于 pendingChecks 而非累积 rebootStatus 决定 reboot).
func (c Client) Reboot() error {
	_, err := c.cominClient.Reboot(context.Background(), &emptypb.Empty{})
	return err
}

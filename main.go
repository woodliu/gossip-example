package main

import (
	"context"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/common/promlog"
	"github.com/woodliu/gossip-example/pkg/cluster"
	_ "go.uber.org/automaxprocs"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

/*
1: 使用 gorilla 作为http，使用shame模块解析数据，使用handler模块记录请求访问
2：使用kubernetes 选举决定主备
3：使用时间轮处理认领的规则，处理周期为6s，时间轮长度为1min，时间片为10个
4：使用gossip实现数据同步
4: 告警信息越界管理
*/

/*
使用环境变量namespace+name+app_id来获取各个endpoint的信息
初始化使用默认的认领时间和聚合时间(或从配置库中获取)，后续可以通过API修改同步相关配置
*/

var promlogConfig = promlog.Config{}

func main() {
	bigArr := make([]byte, 1024*1024*1024)
	runtime.KeepAlive(bigArr)

	//ctx, cancel := context.WithCancel(context.Background())
	//defer cancel()
	//
	//hostName, namespace, labels := cluster.GetEnv()
	//
	peer := cluster.NewPeer() //TODO是否需要等待？
	//go peer.SyncAddr(ctx, hostName, namespace, labels)

	peer.KnownPeers = map[string]struct{}{
		"192.168.80.129:7946": {},
		"192.168.80.131:7946": {},
		"192.168.80.132:7946": {},
	}
	logger := promlog.New(&promlogConfig)
	err := peer.Create(
		log.With(logger, "component", "cluster"),
		cluster.DefaultPushPullInterval,
		cluster.DefaultGossipInterval,
		cluster.DefaultTCPTimeout,
		cluster.DefaultProbeTimeout,
		cluster.DefaultProbeInterval,
	)

	if err != nil {
		level.Warn(logger).Log("msg", "unable to join gossip mesh", "err", err)
	}

	err = peer.Join(
		cluster.DefaultReconnectInterval,
		cluster.DefaultReconnectTimeout,
	)
	if err != nil {
		level.Warn(logger).Log("msg", "unable to join gossip mesh", "err", err)
	}
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), cluster.DefaultPushPullInterval)
	defer func() {
		timeoutCancel()
		if err := peer.Leave(10 * time.Second); err != nil {
			level.Warn(logger).Log("msg", "unable to leave gossip mesh", "err", err)
		}
	}()
	go peer.Settle(timeoutCtx, cluster.DefaultGossipInterval*10)

	go func() {
		for {
			peer.SendData([]byte("hello"))
			time.Sleep(time.Second)
		}
	}()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGKILL, syscall.SIGQUIT)
	<-ch
}

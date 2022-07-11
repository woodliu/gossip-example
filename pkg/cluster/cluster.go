package cluster

import (
	"context"
	"fmt"
	"github.com/go-kit/log"
	"github.com/hashicorp/memberlist"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	DefaultPushPullInterval  = 60 * time.Second
	DefaultGossipInterval    = 200 * time.Millisecond
	DefaultTCPTimeout        = 10 * time.Second
	DefaultProbeTimeout      = 500 * time.Millisecond
	DefaultProbeInterval     = 1 * time.Second
	DefaultReconnectInterval = 10 * time.Second
	DefaultReconnectTimeout  = 6 * time.Hour
	DefaultRefreshInterval   = 15 * time.Second
	MaxGossipPacketSize      = 1400
	DefaultBindAddr          = "0.0.0.0:7946"
	DefaultBindPort          = "7946"
)

// Peer is a single peer in a gossip cluster.
type Peer struct {
	mlist    *memberlist.Memberlist
	delegate *delegate

	resolvedPeers []string //TODO:delete

	mtx    sync.RWMutex
	stopc  chan struct{}
	readyc chan struct{}

	failedPeerLock sync.RWMutex
	peers          sync.Map
	failedPeers    []peer

	peerInformerCh chan PodEvent
	knowPeerLock   sync.RWMutex
	KnownPeers     map[string]struct{} //TODO:小写
	peerChangeCh   chan struct{}

	advertiseAddr atomic.String //myAddress

	failedReconnectionsCounter prometheus.Counter
	reconnectionsCounter       prometheus.Counter
	failedRefreshCounter       prometheus.Counter
	refreshCounter             prometheus.Counter
	peerLeaveCounter           prometheus.Counter
	peerUpdateCounter          prometheus.Counter
	peerJoinCounter            prometheus.Counter
}

func GetEnv() (string, string, map[string]string) {
	hostName, exist := os.LookupEnv(HostnameEnv)
	if !exist {
		panic("need HOSTNAME ENV")
	}

	ns, exist := os.LookupEnv(NamespaceEnv)
	if !exist {
		panic("need NAMESPACE ENV")
	}

	appId, exist := os.LookupEnv(AppidEnv)
	if !exist {
		panic("need APPID ENV")
	}

	return hostName, ns, map[string]string{AppIdKey: appId}
}

func (p *Peer) SyncAddr(ctx context.Context, hostName, namespace string, lbs map[string]string) {
	go p.startInformer(ctx, hostName, namespace, lbs)
	for {
		select {
		case e := <-p.peerInformerCh:
			if e.Action == AddIps {
				p.knowPeerLock.Lock()
				p.KnownPeers[e.Addr] = struct{}{}
				p.knowPeerLock.Unlock()

				p.failedPeerLock.Lock()
				pr, err := getNewPeer(e.Addr)
				if nil == err {
					p.failedPeers = append(p.failedPeers, *pr)
				}
				p.failedPeerLock.Unlock()

				p.peerChangeCh <- struct{}{}
			} else if e.Action == DeleteIps {
				p.knowPeerLock.Lock()
				delete(p.KnownPeers, e.Addr)
				p.knowPeerLock.Unlock()
			}
		case <-ctx.Done():
			close(p.peerChangeCh)
			return
		}
	}
}

func (p *Peer) copyFailedPeers() []peer {
	p.failedPeerLock.RLock()
	defer p.failedPeerLock.RUnlock()

	cp := make([]peer, len(p.failedPeers))
	copy(cp, p.failedPeers)
	return cp
}

func (p *Peer) peerChanged(ctx context.Context) {
	for {
		select {
		case <-p.peerChangeCh:

		case <-ctx.Done():
			return
		}
	}
}

const (
	AddIps = iota
	DeleteIps
)

const (
	AppIdKey     = "app_id"
	HostnameEnv  = "HOSTNAME"
	NamespaceEnv = "NAMESPACE"
	AppidEnv     = "APPID"
)

type PodEvent struct {
	Action int
	Addr   string
}

//注释配置liveness探针确保pod能够正常运行
func (p *Peer) startInformer(ctx context.Context, localHostName, namespace string, lbs map[string]string) {
	//TODO:换成incluster的
	cfg := &rest.Config{
		Host:            "192.168.80.129:6443",
		BearerToken:     "eyJhbGciOiJSUzI1NiIsImtpZCI6Inp3b2t0N0FLWGVyX044NzFENVpHRVVRTWV4Y3g3R0xEZl9WNzE2Xzdaa2cifQ.eyJpc3MiOiJrdWJlcm5ldGVzL3NlcnZpY2VhY2NvdW50Iiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9uYW1lc3BhY2UiOiJkZWZhdWx0Iiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9zZWNyZXQubmFtZSI6InRlc3Qtc2VjcmV0Iiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9zZXJ2aWNlLWFjY291bnQubmFtZSI6InRlc3QiLCJrdWJlcm5ldGVzLmlvL3NlcnZpY2VhY2NvdW50L3NlcnZpY2UtYWNjb3VudC51aWQiOiI5NWE0NjBmNS01MzViLTQ3MDUtYjUwZi02MjQ2MDIwYjYzYmYiLCJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQ6ZGVmYXVsdDp0ZXN0In0.fQ6Vq4vhfbw42G7jI-TI9xly29s_P3levX6TM8WERFOWG5jUOcK6_NWj9vSIPNGtiFSCzHjIBQX0_tPrqvTawfFcyV2NwWMIqURBB0EbEEyQYZxcUEkq9eKr_69RZphnXC7o4kp03RVTr6wZse5MvEgwbdTcjdZaZRZs_YZq4K0OabpdqtmRNvIRLwXfvyJ9WuZXuNl53rJMR7iFfd8gn49qlmChrB_3kEWO84dmRfm9RzjRn8SH90r5ZUcn8g-qhP34JEiVUEjq4PYJ_kNWC5U3BKzFAdYMmTN9PXfe5oMleJ41ztdxwKyrKbx3y7RiRQrwyadmqn-MhTi1kZ_95Q",
		TLSClientConfig: rest.TLSClientConfig{Insecure: true},
	}

	//cfg, err := rest.InClusterConfig()
	//if nil != err {
	//	return err
	//}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		panic(err.Error())
	}

	factory := informers.NewSharedInformerFactoryWithOptions(client, time.Hour, informers.WithNamespace(namespace), informers.WithTweakListOptions(func(options *metav1.ListOptions) {
		options.LabelSelector = labels.SelectorFromSet(lbs).String()
	}))

	podInformer := factory.Core().V1().Pods()
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			if pod.GetName() == localHostName {
				p.advertiseAddr.Store(net.JoinHostPort(pod.Status.PodIP, DefaultBindPort))
				return
			}

			if pod.Status.Phase == corev1.PodRunning {
				p.peerInformerCh <- PodEvent{AddIps, net.JoinHostPort(pod.Status.PodIP, DefaultBindPort)}
				return
			}
		},
		UpdateFunc: func(old, new interface{}) {
			oldPod := old.(*corev1.Pod)
			newPod := new.(*corev1.Pod)
			if newPod.GetName() == localHostName {
				return
			}

			if newPod.Status.Phase == corev1.PodRunning && oldPod.Status.PodIP != newPod.Status.PodIP {
				p.peerInformerCh <- PodEvent{AddIps, net.JoinHostPort(newPod.Status.PodIP, DefaultBindPort)}
				return
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			p.peerInformerCh <- PodEvent{DeleteIps, net.JoinHostPort(pod.Status.PodIP, DefaultBindPort)}
		},
	})

	factory.Start(ctx.Done())
	for informerType, ok := range factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			panic(fmt.Sprintf("Failed to sync cache for %v", informerType))
		}
	}
}

// peer is an internal type used for bookkeeping. It holds the state of peers
// in the cluster.
type peer struct {
	status    PeerStatus
	leaveTime time.Time

	*memberlist.Node
}

// PeerStatus is the state that a peer is in.
type PeerStatus int

type logWriter struct {
	l log.Logger
}

func (l *logWriter) Write(b []byte) (int, error) {
	log.Debugf("memberlist", string(b))
	return len(b), nil
}

func NewPeer() *Peer {
	p := &Peer{
		stopc:          make(chan struct{}),
		readyc:         make(chan struct{}),
		peerInformerCh: make(chan PodEvent),
		peerChangeCh:   make(chan struct{}),
	}

	p.failedReconnectionsCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_reconnections_failed_total",
		Help: "A counter of the number of failed cluster peer reconnection attempts.",
	})

	p.reconnectionsCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_reconnections_total",
		Help: "A counter of the number of cluster peer reconnections.",
	})

	p.failedRefreshCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_refresh_join_failed_total",
		Help: "A counter of the number of failed cluster peer joined attempts via refresh.",
	})
	p.refreshCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_refresh_join_total",
		Help: "A counter of the number of cluster peer joined via refresh.",
	})

	p.peerLeaveCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_peers_left_total",
		Help: "A counter of the number of peers that have left.",
	})
	p.peerUpdateCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_peers_update_total",
		Help: "A counter of the number of peers that have updated metadata.",
	})
	p.peerJoinCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_peers_joined_total",
		Help: "A counter of the number of peers that have joined.",
	})

	return p
}

func (p *Peer) copyPeerAddress() []string {
	copyAddr := make([]string, len(p.KnownPeers))
	i := 0
	p.knowPeerLock.RLock()
	for addr := range p.KnownPeers {
		copyAddr[i] = addr
		i++
	}
	p.knowPeerLock.RUnlock()
	return copyAddr
}

func (p *Peer) Create(
	l log.Logger,
	pushPullInterval time.Duration,
	gossipInterval time.Duration,
	tcpTimeout time.Duration,
	probeTimeout time.Duration,
	probeInterval time.Duration,
) error {
	retransmit := len(p.KnownPeers) / 2
	if retransmit < 3 {
		retransmit = 3
	}
	p.delegate = newDelegate(p, retransmit)

	cfg := memberlist.DefaultLANConfig()
	cfg.Delegate = p.delegate
	cfg.Ping = p.delegate
	cfg.Alive = p.delegate
	cfg.Events = p.delegate
	cfg.GossipInterval = gossipInterval
	cfg.PushPullInterval = pushPullInterval
	cfg.TCPTimeout = tcpTimeout
	cfg.ProbeTimeout = probeTimeout
	cfg.ProbeInterval = probeInterval
	cfg.LogOutput = &logWriter{l: l}
	cfg.GossipNodes = retransmit
	cfg.UDPBufferSize = MaxGossipPacketSize

	p.setInitialFailed(p.copyPeerAddress())

	ml, err := memberlist.Create(cfg)
	if err != nil {
		return errors.Wrap(err, "create memberlist")
	}
	p.mlist = ml
	return nil
}

func getNewPeer(addr string) (*peer, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Don't add textual addresses since memberlist only advertises
		// dotted decimal or IPv6 addresses.
		return nil, err
	}
	portUint, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, err
	}

	return &peer{
		status:    StatusFailed,
		leaveTime: time.Now(),
		Node: &memberlist.Node{
			Addr: ip,
			Port: uint16(portUint),
		},
	}, nil
}

// All peers are initially added to the failed list. They will be removed from
// this list in peerJoin when making their initial connection.
func (p *Peer) setInitialFailed(peers []string) {
	if len(peers) == 0 {
		return
	}

	p.failedPeerLock.Lock()
	defer p.failedPeerLock.Unlock()

	for _, peerAddr := range peers {
		pr, err := getNewPeer(peerAddr)
		if err != nil {
			continue
		}

		p.failedPeers = append(p.failedPeers, *pr)
		p.peers.Store(peerAddr, *pr)
	}
}

func (p *Peer) Join(
	reconnectInterval time.Duration,
	reconnectTimeout time.Duration,
) error {
	n, err := p.mlist.Join(p.copyPeerAddress())
	if err != nil {
		log.Warnf("msg", "failed to join cluster", "err", err)
		if reconnectInterval != 0 {
			log.Infof("msg", fmt.Sprintf("will retry joining cluster every %v", reconnectInterval.String()))
		}
	} else {
		log.Debugf("msg", "joined cluster", "peers", n)
	}

	go func() {
		for range p.peerChangeCh {
			p.reconnect()
		}
	}()

	if reconnectInterval != 0 {
		go p.runPeriodicTask(
			reconnectInterval,
			p.reconnect,
		)
	}
	if reconnectTimeout != 0 {
		go p.runPeriodicTask(
			5*time.Minute,
			func() { p.removeFailedPeers(reconnectTimeout) },
		)
	}
	go p.runPeriodicTask(
		DefaultRefreshInterval,
		p.refresh,
	)

	return err
}

func (p *Peer) runPeriodicTask(d time.Duration, f func()) {
	tick := time.NewTicker(d)
	defer tick.Stop()

	for {
		select {
		case <-p.stopc:
			return
		case <-tick.C:
			f()
		}
	}
}

// 对端未启动时，端口是无法访问的，因此需要一个重连机制来等待对端启动
func (p *Peer) reconnect() {
	failedPeers := p.copyFailedPeers()
	for _, pr := range failedPeers {
		// No need to do book keeping on failedPeers here. If a
		// reconnect is successful, they will be announced in
		// peerJoin().
		if _, err := p.mlist.Join([]string{pr.Address()}); err != nil {
			p.failedReconnectionsCounter.Inc()
			log.Debugf("reconnect result", "failure", "peer", pr.Node, "addr", pr.Address(), "err", err)
		} else {
			p.reconnectionsCounter.Inc()
			log.Debugf("reconnect result", "success", "peer", pr.Node, "addr", pr.Address())
		}
	}
}

func (p *Peer) removeFailedPeers(timeout time.Duration) {
	p.failedPeerLock.Lock()
	defer p.failedPeerLock.Unlock()

	now := time.Now()

	keep := make([]peer, 0, len(p.failedPeers))
	for _, pr := range p.failedPeers {
		if pr.leaveTime.Add(timeout).After(now) {
			keep = append(keep, pr)
		} else {
			log.Debugf("msg", "failed peer has timed out", "peer", pr.Node, "addr", pr.Address())
			p.peers.Delete(pr.Name)
		}
	}

	p.failedPeers = keep
}

func (p *Peer) refresh() {
	resolvedPeers, err := resolvePeers(context.Background(), p.copyPeerAddress(), p.advertiseAddr.Load(), &net.Resolver{}, false)
	if err != nil {
		log.Debugf("peers", p.KnownPeers, "err", err)
		return
	}

	members := p.mlist.Members()
	for _, peer := range resolvedPeers {
		var isPeerFound bool
		for _, member := range members {
			if member.Address() == peer {
				isPeerFound = true
				break
			}
		}

		if !isPeerFound {
			if _, err := p.mlist.Join([]string{peer}); err != nil {
				p.failedRefreshCounter.Inc()
				log.Warnf("refresh result", "failure", "addr", peer, "err", err)
			} else {
				p.refreshCounter.Inc()
				log.Debugf("refresh result", "success", "addr", peer)
			}
		}
	}
}

// Name returns the unique ID of this peer in the cluster.
func (p *Peer) Name() string {
	return p.mlist.LocalNode().Name
}

// Status Return a status string representing the peer state.
func (p *Peer) Status() string {
	if p.Ready() {
		return "ready"
	}

	return "settling"
}

// ClusterMember interface that represents node peers in a cluster
type ClusterMember interface {
	// Name returns the name of the node
	Name() string
	// Address returns the IP address of the node
	Address() string
}

// Member represents a member in the cluster.
type Member struct {
	node *memberlist.Node
}

// Name implements cluster.ClusterMember
func (m Member) Name() string { return m.node.Name }

// Address implements cluster.ClusterMember
func (m Member) Address() string { return m.node.Address() }

// Peers returns the peers in the cluster.
func (p *Peer) Peers() []ClusterMember {
	peers := make([]ClusterMember, 0, len(p.mlist.Members()))
	for _, member := range p.mlist.Members() {
		peers = append(peers, Member{
			node: member,
		})
	}
	return peers
}

// Return true when router has settled.
func (p *Peer) Ready() bool {
	select {
	case <-p.readyc:
		return true
	default:
	}
	return false
}

// Leave the cluster, waiting up to timeout.
func (p *Peer) Leave(timeout time.Duration) error {
	close(p.stopc)
	log.Debugf("msg", "leaving cluster")
	return p.mlist.Leave(timeout)
}

func (p *Peer) SendData(msg []byte) {
	p.peers.Range(func(key, value any) bool {
		pr := value.(peer)
		err := p.mlist.SendReliable(pr.Node, msg)
		if nil != err {
			log.Errorf("send msg to %s failed,err:%s", pr.Name, err.Error())
		}
		return true
	})
}

// ClusterSize returns the current number of alive members in the cluster.
func (p *Peer) ClusterSize() int {
	return p.mlist.NumMembers()
}

//// Self returns the node information about the peer itself.
//func (p *Peer) Self() *memberlist.Node {
//	return p.mlist.LocalNode()
//}

const (
	StatusNone PeerStatus = iota
	StatusAlive
	StatusFailed
)

func (p *Peer) peerJoin(n *memberlist.Node) {
	p.failedPeerLock.Lock()
	defer p.failedPeerLock.Unlock()

	var oldStatus PeerStatus
	obj, ok := p.peers.Load(n.Address())
	pr := obj.(peer)
	if !ok {
		oldStatus = StatusNone
		pr = peer{
			status: StatusAlive,
			Node:   n,
		}
	} else {
		oldStatus = pr.status
		pr.Node = n
		pr.status = StatusAlive
		pr.leaveTime = time.Time{}
	}

	p.peers.Store(n.Address(), pr)
	p.peerJoinCounter.Inc()

	if oldStatus == StatusFailed {
		log.Debugf("msg", "peer rejoined", "peer", pr.Node)
		p.failedPeers = removeOldPeer(p.failedPeers, pr.Address())
	}
}

func removeOldPeer(old []peer, addr string) []peer {
	new := make([]peer, 0, len(old))
	for _, p := range old {
		if p.Address() != addr {
			new = append(new, p)
		}
	}

	return new
}

func (p *Peer) peerLeave(n *memberlist.Node) {
	p.failedPeerLock.Lock()
	defer p.failedPeerLock.Unlock()

	obj, ok := p.peers.Load(n.Address())
	if !ok {
		// Why are we receiving a leave notification from a node that
		// never joined?
		return
	}

	pr := obj.(peer)
	pr.status = StatusFailed
	pr.leaveTime = time.Now()
	p.failedPeers = append(p.failedPeers, pr) //TODO:pod ip不是固定的，是不是要使用informer自动刷新？
	p.peers.Store(n.Address(), pr)

	p.peerLeaveCounter.Inc()
	log.Debugf("msg", "peer left", "peer", pr.Node)
}

func (p *Peer) peerUpdate(n *memberlist.Node) {
	p.failedPeerLock.Lock()
	defer p.failedPeerLock.Unlock()

	obj, ok := p.peers.Load(n.Address())
	if !ok {
		// Why are we receiving an update from a node that never
		// joined?
		return
	}

	pr := obj.(peer)
	pr.Node = n
	p.peers.Store(n.Address(), pr)

	p.peerUpdateCounter.Inc()
	log.Debugf("msg", "peer updated", "peer", pr.Node)
}

// WaitReady Wait until Settle() has finished.
func (p *Peer) WaitReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.readyc:
		return nil
	}
}

// Settle 等待一段时间，用于等待成员加入集群上线  TODO:NumOkayRequired的值需要调整
func (p *Peer) Settle(ctx context.Context, interval time.Duration) {
	const NumOkayRequired = 3
	log.Infof("msg", "Waiting for gossip to settle...", "interval", interval)
	start := time.Now()
	nPeers := 0
	nOkay := 0
	totalPolls := 0
	for {
		select {
		case <-ctx.Done():
			elapsed := time.Since(start)
			log.Infof("msg", "gossip not settled but continuing anyway", "polls", totalPolls, "elapsed", elapsed)
			close(p.readyc)
			return
		case <-time.After(interval):
		}
		elapsed := time.Since(start)
		n := len(p.Peers())
		if nOkay >= NumOkayRequired {
			log.Infof("msg", "gossip settled; proceeding", "elapsed", elapsed)
			break
		}
		if n == nPeers {
			nOkay++
			log.Infof("msg", "gossip looks settled", "elapsed", elapsed)
		} else {
			nOkay = 0
			log.Infof("msg", "gossip not settled", "polls", totalPolls, "before", nPeers, "now", n, "elapsed", elapsed)
		}
		nPeers = n
		totalPolls++
	}
	close(p.readyc)
}

func resolvePeers(ctx context.Context, peers []string, myAddress string, res *net.Resolver, waitIfEmpty bool) ([]string, error) {
	var resolvedPeers []string

	for _, peer := range peers {
		host, port, err := net.SplitHostPort(peer)
		if err != nil {
			return nil, errors.Wrapf(err, "split host/port for peer %s", peer)
		}

		retryCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		ips, err := res.LookupIPAddr(ctx, host)
		if err != nil {
			// Assume direct address.
			resolvedPeers = append(resolvedPeers, peer)
			continue
		}

		if len(ips) == 0 {
			var lookupErrSpotted bool

			err := retry(2*time.Second, retryCtx.Done(), func() error {
				if lookupErrSpotted {
					// We need to invoke cancel in next run of retry when lookupErrSpotted to preserve LookupIPAddr error.
					cancel()
				}

				ips, err = res.LookupIPAddr(retryCtx, host)
				if err != nil {
					lookupErrSpotted = true
					return errors.Wrapf(err, "IP Addr lookup for peer %s", peer)
				}

				ips = removeMyAddr(ips, port, myAddress)
				if len(ips) == 0 {
					if !waitIfEmpty {
						return nil
					}
					return errors.New("empty IPAddr result. Retrying")
				}

				return nil
			})
			if err != nil {
				return nil, err
			}
		}

		for _, ip := range ips {
			resolvedPeers = append(resolvedPeers, net.JoinHostPort(ip.String(), port))
		}
	}

	return resolvedPeers, nil
}

func removeMyAddr(ips []net.IPAddr, targetPort, myAddr string) []net.IPAddr {
	var result []net.IPAddr

	for _, ip := range ips {
		if net.JoinHostPort(ip.String(), targetPort) == myAddr {
			continue
		}
		result = append(result, ip)
	}

	return result
}

//func hasNonlocal(clusterPeers []string) bool {
//	for _, peer := range clusterPeers {
//		if host, _, err := net.SplitHostPort(peer); err == nil {
//			peer = host
//		}
//		if ip := net.ParseIP(peer); ip != nil && !ip.IsLoopback() {
//			return true
//		} else if ip == nil && strings.ToLower(peer) != "localhost" {
//			return true
//		}
//	}
//	return false
//}
//
//func isUnroutable(addr string) bool {
//	if host, _, err := net.SplitHostPort(addr); err == nil {
//		addr = host
//	}
//	if ip := net.ParseIP(addr); ip != nil && (ip.IsUnspecified() || ip.IsLoopback()) {
//		return true // typically 0.0.0.0 or localhost
//	} else if ip == nil && strings.ToLower(addr) == "localhost" {
//		return true
//	}
//	return false
//}
//
//func isAny(addr string) bool {
//	if host, _, err := net.SplitHostPort(addr); err == nil {
//		addr = host
//	}
//	return addr == "" || net.ParseIP(addr).IsUnspecified()
//}

// retry executes f every interval seconds until timeout or no error is returned from f.
func retry(interval time.Duration, stopc <-chan struct{}, f func() error) error {
	tick := time.NewTicker(interval)
	defer tick.Stop()

	var err error
	for {
		if err = f(); err == nil {
			return nil
		}
		select {
		case <-stopc:
			return err
		case <-tick.C:
		}
	}
}

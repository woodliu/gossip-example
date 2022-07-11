package cluster

import (
	"fmt"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/hashicorp/memberlist"
	"github.com/prometheus/client_golang/prometheus"
	"time"
)

type delegate struct {
	*Peer

	logger log.Logger

	messagesReceived     *prometheus.CounterVec
	messagesReceivedSize *prometheus.CounterVec
	messagesSent         *prometheus.CounterVec
	messagesSentSize     *prometheus.CounterVec
	messagesPruned       prometheus.Counter
	nodeAlive            *prometheus.CounterVec
	nodePingDuration     *prometheus.HistogramVec
}

const (
	// Maximum number of messages to be held in the queue.
	maxQueueSize = 4096
	fullState    = "full_state"
	update       = "update"
)

func newDelegate(p *Peer, retransmit int) *delegate {
	messagesReceived := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "alertmanager_cluster_messages_received_total",
		Help: "Total number of cluster messages received.",
	}, []string{"msg_type"})
	messagesReceivedSize := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "alertmanager_cluster_messages_received_size_total",
		Help: "Total size of cluster messages received.",
	}, []string{"msg_type"})
	messagesSent := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "alertmanager_cluster_messages_sent_total",
		Help: "Total number of cluster messages sent.",
	}, []string{"msg_type"})
	messagesSentSize := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "alertmanager_cluster_messages_sent_size_total",
		Help: "Total size of cluster messages sent.",
	}, []string{"msg_type"})
	messagesPruned := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_cluster_messages_pruned_total",
		Help: "Total number of cluster messages pruned.",
	})
	//gossipClusterMembers := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
	//	Name: "alertmanager_cluster_members",
	//	Help: "Number indicating current number of members in cluster.",
	//}, func() float64 {
	//	return float64(p.ClusterSize())
	//})
	//healthScore := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
	//	Name: "alertmanager_cluster_health_score",
	//	Help: "Health score of the cluster. Lower values are better and zero means 'totally healthy'.",
	//}, func() float64 {
	//	return float64(p.mlist.GetHealthScore())
	//})
	//messagesQueued := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
	//	Name: "alertmanager_cluster_messages_queued",
	//	Help: "Number of cluster messages which are queued.",
	//}, func() float64 {
	//	return float64(bcast.NumQueued())
	//})
	nodeAlive := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "alertmanager_cluster_alive_messages_total",
		Help: "Total number of received alive messages.",
	}, []string{"peer"},
	)
	nodePingDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "alertmanager_cluster_pings_seconds",
		Help:    "Histogram of latencies for ping messages.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5},
	}, []string{"peer"},
	)

	messagesReceived.WithLabelValues(fullState)
	messagesReceivedSize.WithLabelValues(fullState)
	messagesReceived.WithLabelValues(update)
	messagesReceivedSize.WithLabelValues(update)
	messagesSent.WithLabelValues(fullState)
	messagesSentSize.WithLabelValues(fullState)
	messagesSent.WithLabelValues(update)
	messagesSentSize.WithLabelValues(update)

	d := &delegate{
		Peer: p,

		messagesReceived:     messagesReceived,
		messagesReceivedSize: messagesReceivedSize,
		messagesSent:         messagesSent,
		messagesSentSize:     messagesSentSize,
		messagesPruned:       messagesPruned,
		nodeAlive:            nodeAlive,
		nodePingDuration:     nodePingDuration,
	}

	//go d.handleQueueDepth()

	return d
}

// NodeMeta retrieves meta-data about the current node when broadcasting an alive message.
func (d *delegate) NodeMeta(limit int) []byte {
	return []byte{}
}

//NotifyMsg is the callback invoked when a user-level gossip message is received.
func (d *delegate) NotifyMsg(b []byte) { //TODO接收数据即可
	d.messagesReceived.WithLabelValues(update).Inc()
	d.messagesReceivedSize.WithLabelValues(update).Add(float64(len(b)))
	fmt.Println(string(b))

	//var p clusterpb.Part
	//if err := proto.Unmarshal(b, &p); err != nil {
	//	log.Warnf("msg", "decode broadcast", "err", err)
	//	return
	//}
	//
	//d.mtx.RLock()
	//s, ok := d.states[p.Key]
	//d.mtx.RUnlock()
	//
	//if !ok {
	//	return
	//}
	//if err := s.Merge(p.Data); err != nil {
	//	log.Warnf("msg", "merge broadcast", "err", err, "key", p.Key)
	//	return
	//}
}

// GetBroadcasts is called when user data messages can be broadcasted.
func (d *delegate) GetBroadcasts(overhead, limit int) [][]byte {
	return nil
}

//LocalState is called when gossip fetches local state.
func (d *delegate) LocalState(_ bool) []byte {
	return nil
}

func (d *delegate) MergeRemoteState(buf []byte, _ bool) {
	return
}

// NotifyJoin is called if a peer joins the cluster.
func (d *delegate) NotifyJoin(n *memberlist.Node) {
	level.Debug(d.logger).Log("received", "NotifyJoin", "node", n.Name, "addr", n.Address())
	d.Peer.peerJoin(n)
}

// NotifyLeave is called if a peer leaves the cluster.
func (d *delegate) NotifyLeave(n *memberlist.Node) {
	level.Debug(d.logger).Log("received", "NotifyLeave", "node", n.Name, "addr", n.Address())
	d.Peer.peerLeave(n)
}

// NotifyUpdate is called if a cluster peer gets updated.
func (d *delegate) NotifyUpdate(n *memberlist.Node) {
	level.Debug(d.logger).Log("received", "NotifyUpdate", "node", n.Name, "addr", n.Address())
	d.Peer.peerUpdate(n)
}

// NotifyAlive implements the memberlist.AliveDelegate interface.
func (d *delegate) NotifyAlive(peer *memberlist.Node) error {
	d.nodeAlive.WithLabelValues(peer.Name).Inc()
	return nil
}

// AckPayload implements the memberlist.PingDelegate interface.
func (d *delegate) AckPayload() []byte {
	return []byte{}
}

// NotifyPingComplete implements the memberlist.PingDelegate interface.
func (d *delegate) NotifyPingComplete(peer *memberlist.Node, rtt time.Duration, payload []byte) {
	d.nodePingDuration.WithLabelValues(peer.Name).Observe(rtt.Seconds())
}

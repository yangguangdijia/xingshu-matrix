package middleware

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// kafkaAdmin builds a kadm client from the instance (seed brokers, TLS, SASL).
func kafkaAdmin(inst *Instance) (*kadm.Client, error) {
	opts := []kgo.Opt{kgo.SeedBrokers(inst.Endpoints...), kgo.DialTimeout(5 * time.Second), kgo.RequestRetries(1),
		kgo.ClientID("xingshu-matrix")}
	if inst.Auth.TLS {
		opts = append(opts, kgo.DialTLSConfig(&tls.Config{InsecureSkipVerify: inst.Auth.TLSSkipVerify})) //nolint:gosec // user-controlled per instance
	} else {
		opts = append(opts, kgo.Dialer((&net.Dialer{Timeout: 5 * time.Second}).DialContext))
	}
	u, p := inst.Auth.Username, inst.Auth.Password
	switch strings.ToUpper(inst.Auth.SASLMechanism) {
	case "":
	case "PLAIN":
		opts = append(opts, kgo.SASL(plain.Auth{User: u, Pass: p}.AsMechanism()))
	case "SCRAM-SHA-256":
		opts = append(opts, kgo.SASL(scram.Auth{User: u, Pass: p}.AsSha256Mechanism()))
	case "SCRAM-SHA-512":
		opts = append(opts, kgo.SASL(scram.Auth{User: u, Pass: p}.AsSha512Mechanism()))
	default:
		return nil, httpx.Validation("unsupported sasl_mechanism " + inst.Auth.SASLMechanism)
	}
	return kadm.NewOptClient(opts...)
}

func withKafka(c *gin.Context, inst *Instance, fn func(ctx context.Context, adm *kadm.Client) (any, error)) {
	adm, err := kafkaAdmin(inst)
	if err != nil {
		upstream(c, err)
		return
	}
	defer adm.Close()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	v, err := fn(ctx, adm)
	if err != nil {
		upstream(c, err)
		return
	}
	httpx.OK(c, v)
}

// BrokerInfo is a Kafka broker.
type BrokerInfo struct {
	NodeID int32  `json:"node_id"`
	Host   string `json:"host"`
	Port   int32  `json:"port"`
	Rack   string `json:"rack"`
}

// TopicInfo is a Kafka topic summary.
type TopicInfo struct {
	Name              string          `json:"name"`
	ID                string          `json:"id"`
	Internal          bool            `json:"internal"`
	Partitions        int             `json:"partitions"`
	ReplicationFactor int             `json:"replication_factor"`
	UnderReplicated   int             `json:"under_replicated"`
	Error             string          `json:"error,omitempty"`
	PartitionDetails  []PartitionInfo `json:"partition_details,omitempty"`
	Configs           []TopicConfig   `json:"configs,omitempty"`
}

type PartitionInfo struct {
	Partition int32   `json:"partition"`
	Leader    int32   `json:"leader"`
	Replicas  []int32 `json:"replicas"`
	ISR       []int32 `json:"isr"`
	Offline   []int32 `json:"offline_replicas"`
	Start     int64   `json:"start_offset"`
	End       int64   `json:"end_offset"`
}

type TopicConfig struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Source    string `json:"source"`
	Sensitive bool   `json:"sensitive"`
}

func toTopicInfo(t kadm.TopicDetail, detail bool) TopicInfo {
	ti := TopicInfo{Name: t.Topic, ID: t.ID.String(), Internal: t.IsInternal, Partitions: len(t.Partitions), ReplicationFactor: t.Partitions.NumReplicas()}
	if t.Err != nil {
		ti.Error = t.Err.Error()
	}
	for _, p := range t.Partitions.Sorted() {
		if len(p.ISR) < len(p.Replicas) {
			ti.UnderReplicated++
		}
		if detail {
			ti.PartitionDetails = append(ti.PartitionDetails, PartitionInfo{Partition: p.Partition, Leader: p.Leader, Replicas: p.Replicas, ISR: p.ISR, Offline: p.OfflineReplicas})
		}
	}
	return ti
}

func (m *Module) kafkaBrokers(c *gin.Context) {
	inst, ok := m.instanceFor(c, TypeKafka)
	if !ok {
		return
	}
	withKafka(c, inst, func(ctx context.Context, adm *kadm.Client) (any, error) {
		bs, err := adm.ListBrokers(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]BrokerInfo, 0, len(bs))
		for _, b := range bs {
			bi := BrokerInfo{NodeID: b.NodeID, Host: b.Host, Port: b.Port}
			if b.Rack != nil {
				bi.Rack = *b.Rack
			}
			out = append(out, bi)
		}
		return out, nil
	})
}

func (m *Module) kafkaTopics(c *gin.Context) {
	inst, ok := m.instanceFor(c, TypeKafka)
	if !ok {
		return
	}
	internal := c.Query("internal") == "true"
	kw := c.Query("keyword")
	withKafka(c, inst, func(ctx context.Context, adm *kadm.Client) (any, error) {
		var tds kadm.TopicDetails
		var err error
		if internal {
			tds, err = adm.ListTopicsWithInternal(ctx)
		} else {
			tds, err = adm.ListTopics(ctx)
		}
		if err != nil {
			return nil, err
		}
		out := []TopicInfo{}
		for _, t := range tds.Sorted() {
			if kw == "" || strings.Contains(t.Topic, kw) {
				out = append(out, toTopicInfo(t, false))
			}
		}
		return out, nil
	})
}

func (m *Module) kafkaTopic(c *gin.Context) {
	inst, ok := m.instanceFor(c, TypeKafka)
	if !ok {
		return
	}
	topic := c.Param("topic")
	withKafka(c, inst, func(ctx context.Context, adm *kadm.Client) (any, error) {
		tds, err := adm.ListTopicsWithInternal(ctx, topic)
		if err != nil {
			return nil, err
		}
		td, ok := tds[topic]
		if !ok || td.Err != nil {
			return nil, httpx.NotFound("topic not found: " + topic)
		}
		ti := toTopicInfo(td, true)
		starts, _ := adm.ListStartOffsets(ctx, topic)
		ends, _ := adm.ListEndOffsets(ctx, topic)
		for i := range ti.PartitionDetails {
			p := ti.PartitionDetails[i].Partition
			if o, ok := starts.Lookup(topic, p); ok {
				ti.PartitionDetails[i].Start = o.Offset
			}
			if o, ok := ends.Lookup(topic, p); ok {
				ti.PartitionDetails[i].End = o.Offset
			}
		}
		if rcs, err := adm.DescribeTopicConfigs(ctx, topic); err == nil {
			for _, rc := range rcs {
				for _, cf := range rc.Configs {
					tc := TopicConfig{Key: cf.Key, Source: cf.Source.String(), Sensitive: cf.Sensitive}
					if cf.Value != nil && !cf.Sensitive {
						tc.Value = *cf.Value
					}
					ti.Configs = append(ti.Configs, tc)
				}
			}
			sort.Slice(ti.Configs, func(i, j int) bool { return ti.Configs[i].Key < ti.Configs[j].Key })
		}
		return ti, nil
	})
}

type createTopicInput struct {
	Name              string            `json:"name" binding:"required,max=249"`
	Partitions        int32             `json:"partitions"`
	ReplicationFactor int16             `json:"replication_factor"`
	Configs           map[string]string `json:"configs"`
}

func (m *Module) kafkaCreateTopic(c *gin.Context) {
	inst, ok := m.instanceFor(c, TypeKafka)
	if !ok {
		return
	}
	var in createTopicInput
	if !httpx.Bind(c, &in) {
		return
	}
	if in.Partitions <= 0 {
		in.Partitions = -1 // broker default (Kafka 2.4+)
	}
	if in.ReplicationFactor <= 0 {
		in.ReplicationFactor = -1
	}
	cfg := map[string]*string{}
	for k, v := range in.Configs {
		v := v
		cfg[k] = &v
	}
	m.runChange(c, inst, change{Op: "kafka.create_topic", InstanceID: inst.ID, Topic: in.Name, Partitions: in.Partitions,
		ReplicationFactor: in.ReplicationFactor, Configs: cfg}, "创建 Kafka Topic "+in.Name)
}

func (m *Module) kafkaDeleteTopic(c *gin.Context) {
	inst, ok := m.instanceFor(c, TypeKafka)
	if !ok {
		return
	}
	topic := c.Param("topic")
	m.runChange(c, inst, change{Op: "kafka.delete_topic", InstanceID: inst.ID, Topic: topic}, "删除 Kafka Topic "+topic)
}

func kafkaCreateTopic(ctx context.Context, inst *Instance, ch change) (any, error) {
	adm, err := kafkaAdmin(inst)
	if err != nil {
		return nil, err
	}
	defer adm.Close()
	resp, err := adm.CreateTopic(ctx, ch.Partitions, ch.ReplicationFactor, ch.Configs, ch.Topic)
	if err != nil {
		return nil, err
	}
	if resp.Err != nil {
		return nil, fmt.Errorf("%v %s", resp.Err, resp.ErrMessage)
	}
	return gin.H{"topic": resp.Topic, "partitions": resp.NumPartitions, "replication_factor": resp.ReplicationFactor}, nil
}

func kafkaDeleteTopic(ctx context.Context, inst *Instance, topic string) (any, error) {
	adm, err := kafkaAdmin(inst)
	if err != nil {
		return nil, err
	}
	defer adm.Close()
	resp, err := adm.DeleteTopic(ctx, topic)
	if err != nil {
		return nil, err
	}
	if resp.Err != nil {
		return nil, fmt.Errorf("%v %s", resp.Err, resp.ErrMessage)
	}
	return gin.H{"topic": topic, "deleted": true}, nil
}

// GroupLagInfo is a consumer group with its lag.
type GroupLagInfo struct {
	Group        string           `json:"group"`
	State        string           `json:"state"`
	ProtocolType string           `json:"protocol_type"`
	Protocol     string           `json:"protocol"`
	Coordinator  int32            `json:"coordinator"`
	Members      int              `json:"members"`
	TotalLag     int64            `json:"total_lag"`
	TopicLag     map[string]int64 `json:"topic_lag"`
	Partitions   []PartitionLag   `json:"partitions,omitempty"`
	Error        string           `json:"error,omitempty"`
}

type PartitionLag struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Committed int64  `json:"committed"`
	End       int64  `json:"end_offset"`
	Lag       int64  `json:"lag"`
	Member    string `json:"member,omitempty"`
	ClientID  string `json:"client_id,omitempty"`
	Host      string `json:"host,omitempty"`
}

func (m *Module) kafkaGroups(c *gin.Context) {
	inst, ok := m.instanceFor(c, TypeKafka)
	if !ok {
		return
	}
	var groups []string
	if g := c.Query("group"); g != "" {
		groups = []string{g}
	}
	v, err := kafkaGroupLags(c.Request.Context(), inst, groups, c.Query("detail") == "true" || len(groups) == 1)
	if err != nil {
		upstream(c, err)
		return
	}
	httpx.OK(c, v)
}

// kafkaGroupLags describes groups (all when empty) and computes lag via kadm.Lag.
func kafkaGroupLags(ctx context.Context, inst *Instance, groups []string, detail bool) ([]GroupLagInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	adm, err := kafkaAdmin(inst)
	if err != nil {
		return nil, err
	}
	defer adm.Close()
	if len(groups) == 0 {
		lg, err := adm.ListGroups(ctx)
		if err != nil {
			return nil, err
		}
		groups = lg.Groups()
	}
	out := []GroupLagInfo{}
	if len(groups) == 0 {
		return out, nil
	}
	lags, err := adm.Lag(ctx, groups...)
	if err != nil {
		return nil, err
	}
	for _, g := range lags.Sorted() {
		gi := GroupLagInfo{Group: g.Group, State: g.State, ProtocolType: g.ProtocolType, Protocol: g.Protocol,
			Coordinator: g.Coordinator.NodeID, Members: len(g.Members), TopicLag: map[string]int64{}}
		if e := g.Error(); e != nil {
			gi.Error = e.Error()
		}
		gi.TotalLag = g.Lag.Total()
		for _, tl := range g.Lag.TotalByTopic() {
			gi.TopicLag[tl.Topic] = tl.Lag
		}
		if detail {
			for _, pl := range g.Lag.Sorted() {
				p := PartitionLag{Topic: pl.Topic, Partition: pl.Partition, Committed: pl.Commit.At, End: pl.End.Offset, Lag: pl.Lag}
				if pl.Member != nil {
					p.Member, p.ClientID, p.Host = pl.Member.MemberID, pl.Member.ClientID, pl.Member.ClientHost
				}
				gi.Partitions = append(gi.Partitions, p)
			}
		}
		out = append(out, gi)
	}
	return out, nil
}

func kafkaOverview(ctx context.Context, inst *Instance) (any, error) {
	adm, err := kafkaAdmin(inst)
	if err != nil {
		return nil, err
	}
	defer adm.Close()
	bs, err := adm.ListBrokers(ctx)
	if err != nil {
		return nil, err
	}
	tds, err := adm.ListTopics(ctx)
	if err != nil {
		return nil, err
	}
	parts, under := 0, 0
	for _, t := range tds {
		ti := toTopicInfo(t, false)
		parts += ti.Partitions
		under += ti.UnderReplicated
	}
	lg, err := adm.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	var totalLag int64
	top := []GroupLagInfo{}
	if gs := lg.Groups(); len(gs) > 0 {
		if lags, err := adm.Lag(ctx, gs...); err == nil {
			for _, g := range lags.Sorted() {
				l := g.Lag.Total()
				totalLag += l
				top = append(top, GroupLagInfo{Group: g.Group, State: g.State, Members: len(g.Members), TotalLag: l})
			}
			sort.Slice(top, func(i, j int) bool { return top[i].TotalLag > top[j].TotalLag })
			if len(top) > 10 {
				top = top[:10]
			}
		}
	}
	return gin.H{"type": TypeKafka, "instance": inst.Name, "brokers": len(bs), "topics": len(tds), "partitions": parts,
		"under_replicated_partitions": under, "consumer_groups": len(lg), "total_lag": totalLag, "top_lag_groups": top}, nil
}

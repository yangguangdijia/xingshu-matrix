package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// dedupLimiter is the in-process half of "same event at most once per N
// minutes per channel"; the records table is the cross-replica half.
type dedupLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newDedupLimiter() *dedupLimiter { return &dedupLimiter{last: map[string]time.Time{}} }

// hit reports whether key was sent within window; when it was not, the key is
// marked as sent at now (call forget if the send then fails).
func (l *dedupLimiter) hit(key string, window time.Duration, now time.Time) bool {
	if window <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[key]; ok && now.Sub(t) < window {
		return true
	}
	l.last[key] = now
	if len(l.last) > 20000 { // bounded memory: drop entries older than a day
		for k, t := range l.last {
			if now.Sub(t) > 24*time.Hour {
				delete(l.last, k)
			}
		}
	}
	return false
}

func (l *dedupLimiter) forget(key string) {
	l.mu.Lock()
	delete(l.last, key)
	l.mu.Unlock()
}

var notifyLimiter = newDedupLimiter()

// channelAccepts applies the channel's own receive range. It only filters
// alerts (the legacy behaviour); other event kinds are routed by subscriptions.
func channelAccepts(ch *Channel, ev *NotifyEvent) bool {
	if !ch.Enabled {
		return false
	}
	if ev.Kind != "alert" {
		return true
	}
	if len(ch.Severities) > 0 && !inList(ch.Severities, ev.Severity) && !inList(ch.Severities, ev.SeverityRaw) {
		return false
	}
	return len(ch.Envs) == 0 || inList(ch.Envs, ev.Env)
}

// delivery is one planned send: an event (or an aggregated group) to a channel via a subscription.
type delivery struct {
	sub    *Subscription
	ch     *Channel
	events []*NotifyEvent
}

// planDeliveries routes events to channels. Each event reaches a channel at
// most once even if several subscriptions match. Suppressed sends (quiet hours,
// rate limit) are returned separately so they can be recorded.
func planDeliveries(events []*NotifyEvent, subs []*Subscription, chans map[int64]*Channel, now time.Time,
	recently func(sub *Subscription, ch *Channel, ev *NotifyEvent) bool) (out []*delivery, suppressed []*notifyRecord) {
	groups := map[string]*delivery{}
	for _, ev := range events {
		sent := map[int64]bool{}
		for _, s := range subs {
			if !s.Match(ev) {
				continue
			}
			quiet := s.Quiet.active(now) && !(s.Quiet.AllowCritical && ev.Severity == "critical")
			for _, id := range s.ChannelIDs {
				ch := chans[id]
				if ch == nil || sent[id] || !channelAccepts(ch, ev) {
					continue
				}
				sent[id] = true
				reason := ""
				if quiet {
					reason = "处于静默时段（" + s.Quiet.Start + "-" + s.Quiet.End + "）"
				} else if s.DedupMinutes > 0 && recently != nil && recently(s, ch, ev) {
					reason = fmt.Sprintf("限频：%d 分钟内已发送过相同事件", s.DedupMinutes)
				}
				if reason != "" {
					r := recordFor(s, ch, ev)
					r.Status, r.Reason = "suppressed", reason
					suppressed = append(suppressed, r)
					continue
				}
				if s.Aggregate {
					k := strconv.FormatInt(s.ID, 10) + "/" + strconv.FormatInt(id, 10) + "/" + ev.Kind
					if g := groups[k]; g != nil {
						g.events = append(g.events, ev)
						continue
					}
					g := &delivery{sub: s, ch: ch, events: []*NotifyEvent{ev}}
					groups[k] = g
					out = append(out, g)
					continue
				}
				out = append(out, &delivery{sub: s, ch: ch, events: []*NotifyEvent{ev}})
			}
		}
	}
	return out, suppressed
}

func recordFor(s *Subscription, ch *Channel, ev *NotifyEvent) *notifyRecord {
	r := &notifyRecord{ChannelID: ch.ID, ChannelName: ch.Name, ChannelType: ch.Type, EventKind: ev.Kind,
		EventStatus: ev.Status, EventKey: ev.Key, Severity: ev.Severity, Env: ev.Env, Cluster: ev.Cluster,
		Title: ev.AlertName, TriggeredBy: "system", event: ev}
	if s != nil {
		r.SubscriptionID, r.SubscriptionName = &s.ID, s.Name
	}
	return r
}

// renderFor renders ev with the channel's chosen template for its kind, or the built-in default.
func (m *Module) renderFor(ctx context.Context, ch *Channel, ev *NotifyEvent) (string, string, error) {
	b := builtinTemplates[ev.Kind]
	titleTpl, bodyTpl := b.TitleTpl, b.BodyTpl
	if id := ch.Templates[ev.Kind]; id > 0 && m.d != nil && m.d.DB != nil {
		if t, err := m.loadTemplate(ctx, id); err == nil {
			titleTpl, bodyTpl = t.TitleTpl, t.BodyTpl
		}
	}
	title, body, err := renderTemplate(titleTpl, bodyTpl, ev)
	if err != nil && (titleTpl != b.TitleTpl || bodyTpl != b.BodyTpl) {
		// a broken custom template must not swallow the notification
		title, body, err = renderTemplate(b.TitleTpl, b.BodyTpl, ev)
		if err == nil {
			body += "\n（自定义模板渲染失败，已使用默认模板）"
		}
	}
	if err == nil && ev.extra != "" {
		body += ev.extra
	}
	return title, body, err
}

// renderGroup renders an aggregated delivery as one message.
func (m *Module) renderGroup(ctx context.Context, d *delivery) (string, string, error) {
	if len(d.events) == 1 {
		return m.renderFor(ctx, d.ch, d.events[0])
	}
	counts := map[string]int{}
	var b strings.Builder
	const maxItems = 10
	for i, ev := range d.events {
		counts[or(statusText, ev.Status)]++
		if i >= maxItems {
			continue
		}
		t, body, err := m.renderFor(ctx, d.ch, ev)
		if err != nil {
			return "", "", err
		}
		if i > 0 {
			b.WriteString("\n\n———————\n")
		}
		fmt.Fprintf(&b, "%d. %s\n%s", i+1, t, body)
	}
	if n := len(d.events) - maxItems; n > 0 {
		fmt.Fprintf(&b, "\n\n其余 %d 条已省略，请到平台查看。", n)
	}
	parts := make([]string, 0, len(counts))
	for _, k := range []string{"触发", "已恢复", "已声明", "已指派", "已认领", "成功", "失败", "已取消", "待审批"} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
		}
	}
	kind := or(kindText, d.events[0].Kind)
	title := fmt.Sprintf("【%s汇总】共 %d 条（%s）", kind, len(d.events), strings.Join(parts, " / "))
	return title, b.String(), nil
}

// dispatch routes, renders, sends and records events. Safe to call from any goroutine.
func (m *Module) dispatch(ctx context.Context, events []*NotifyEvent) {
	if len(events) == 0 || m.d == nil || m.d.DB == nil {
		return
	}
	subs, err := m.loadSubs(ctx, true)
	if err != nil {
		m.logWarn("load notify subscriptions", err)
		return
	}
	chans, err := m.enabledChannels(ctx)
	if err != nil {
		m.logWarn("load channels", err)
		return
	}
	now := time.Now()
	recently := func(s *Subscription, ch *Channel, ev *NotifyEvent) bool {
		win := time.Duration(s.DedupMinutes) * time.Minute
		if notifyLimiter.hit(limiterKey(s, ch, ev), win, now) {
			return true
		}
		var hit bool
		_ = m.d.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM obs_notify_records WHERE channel_id=$1 AND event_key=$2 AND status='success'
			AND created_at > now() - make_interval(mins => $3))`, ch.ID, ev.Key, s.DedupMinutes).Scan(&hit)
		return hit
	}
	plan, suppressed := planDeliveries(events, subs, chans, now, recently)
	for _, r := range suppressed {
		m.saveRecord(ctx, r)
	}
	snd := m.sender(ctx)
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, d := range plan {
		wg.Add(1)
		sem <- struct{}{}
		go func(d *delivery) {
			defer func() { <-sem; wg.Done() }()
			m.deliver(ctx, snd, d)
		}(d)
	}
	wg.Wait()
}

func limiterKey(s *Subscription, ch *Channel, ev *NotifyEvent) string {
	return strconv.FormatInt(s.ID, 10) + "|" + strconv.FormatInt(ch.ID, 10) + "|" + ev.Key
}

func (m *Module) deliver(ctx context.Context, snd *sender, d *delivery) {
	first := d.events[0]
	rec := recordFor(d.sub, d.ch, first)
	if len(d.events) > 1 {
		keys := make([]string, 0, len(d.events))
		for _, e := range d.events {
			keys = append(keys, e.Key)
		}
		rec.EventKey = firstN("agg:"+strings.Join(keys, ","), 500)
		rec.EventStatus = "aggregated"
		rec.event = nil
	}
	title, body, err := m.renderGroup(ctx, d)
	if err == nil {
		rec.Title, rec.Body = title, body
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		start := time.Now()
		var msgEv *NotifyEvent
		if len(d.events) == 1 {
			msgEv = first
		}
		err = snd.send(sctx, d.ch, Message{Title: title, Body: body, Event: msgEv})
		cancel()
		rec.DurationMS = int(time.Since(start).Milliseconds())
	}
	rec.setResult(err)
	if err != nil {
		for _, e := range d.events {
			notifyLimiter.forget(limiterKey(d.sub, d.ch, e))
		}
		m.logWarn("send notification", fmt.Errorf("channel %s event %s: %w", d.ch.Name, first.Key, err))
	}
	m.saveRecord(ctx, rec)
}

func (m *Module) enabledChannels(ctx context.Context) (map[int64]*Channel, error) {
	rows, err := m.d.DB.Query(ctx, `SELECT `+chCols+` FROM obs_channels WHERE enabled`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*Channel{}
	for rows.Next() {
		ch, err := m.scanChannel(rows)
		if err != nil {
			m.logWarn("decode channel", err)
			continue
		}
		out[ch.ID] = ch
	}
	return out, rows.Err()
}

// notifyAlerts sends alerts (newly firing, or resolved when the caller passes
// them) through the subscription router. It keeps the escalation and AIOps
// diagnosis hooks that ran here before.
func (m *Module) notifyAlerts(ctx context.Context, alerts []Alert) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	m.escalate(ctx, alerts)
	policy := m.aiopsPolicy(ctx)
	events := make([]*NotifyEvent, 0, len(alerts))
	for i := range alerts {
		a := &alerts[i]
		if policy.AutoDiagnose && a.Status == "firing" && a.FireCount > 0 {
			_, _ = m.queueDiagnosis(ctx, a, false)
		}
		link := ""
		if policy.PublicURL != "" {
			link = strings.TrimRight(policy.PublicURL, "/") + "/#/observe/alerts/" + strconv.FormatInt(a.ID, 10)
		}
		ev := alertEvent(*a, link)
		if d, err := m.diagnosis(ctx, a); err == nil {
			ev.extra = diagnosisNotification(d, policy)
		}
		events = append(events, &ev)
	}
	m.dispatch(ctx, events)
}

func (m *Module) logWarn(msg string, err error) {
	if m.d != nil && m.d.Logger != nil {
		m.d.Logger.Warn(msg, "err", err)
	}
}

// ------------------------------------------------------------------ records

type notifyRecord struct {
	ID               int64           `json:"id"`
	ChannelID        int64           `json:"channel_id"`
	ChannelName      string          `json:"channel_name"`
	ChannelType      string          `json:"channel_type"`
	SubscriptionID   *int64          `json:"subscription_id"`
	SubscriptionName string          `json:"subscription_name"`
	EventKind        string          `json:"event_kind"`
	EventStatus      string          `json:"event_status"`
	EventKey         string          `json:"event_key"`
	Severity         string          `json:"severity"`
	Env              string          `json:"env"`
	Cluster          string          `json:"cluster"`
	Title            string          `json:"title"`
	Body             string          `json:"body,omitempty"`
	EventJSON        json.RawMessage `json:"event,omitempty"`
	Status           string          `json:"status"`
	Reason           string          `json:"reason"`
	DurationMS       int             `json:"duration_ms"`
	ResendOf         *int64          `json:"resend_of"`
	TriggeredBy      string          `json:"triggered_by"`
	CreatedAt        time.Time       `json:"created_at"`

	event *NotifyEvent
}

func (r *notifyRecord) setResult(err error) {
	if err != nil {
		r.Status, r.Reason = "failed", firstN(err.Error(), 1000)
		return
	}
	r.Status, r.Reason = "success", ""
}

func (m *Module) saveRecord(ctx context.Context, r *notifyRecord) {
	if m.d == nil || m.d.DB == nil {
		return
	}
	evJSON := []byte("{}")
	if r.event != nil {
		evJSON, _ = json.Marshal(r.event)
	} else if len(r.EventJSON) > 0 {
		evJSON = r.EventJSON
	}
	err := m.d.DB.QueryRow(ctx, `INSERT INTO obs_notify_records (channel_id,channel_name,channel_type,subscription_id,subscription_name,event_kind,event_status,
		event_key,severity,env,cluster,title,body,event_json,status,reason,duration_ms,resend_of,triggered_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19) RETURNING id, created_at`,
		r.ChannelID, r.ChannelName, r.ChannelType, r.SubscriptionID, r.SubscriptionName, r.EventKind, r.EventStatus,
		firstN(r.EventKey, 500), r.Severity, r.Env, r.Cluster, firstN(r.Title, 500), firstN(r.Body, 20000), evJSON, r.Status, r.Reason,
		r.DurationMS, r.ResendOf, r.TriggeredBy).Scan(&r.ID, &r.CreatedAt)
	if err != nil {
		m.logWarn("save notify record", err)
		return
	}
	if r.Status != "suppressed" && r.ChannelID > 0 {
		_, _ = m.d.DB.Exec(ctx, `UPDATE obs_channels SET last_sent_at=now(), last_status=$2 WHERE id=$1`, r.ChannelID, r.Status)
	}
}

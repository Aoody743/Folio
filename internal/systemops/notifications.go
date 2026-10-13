package systemops

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type SMTPConfig struct {
	Host       string   `json:"host"`
	Port       int      `json:"port"`
	TLSMode    string   `json:"tls_mode"`
	Username   string   `json:"username,omitempty"`
	From       string   `json:"from"`
	Recipients []string `json:"recipients"`
}
type Channel struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Kind      string      `json:"kind"`
	SMTP      *SMTPConfig `json:"smtp,omitempty"`
	ChatID    string      `json:"chat_id,omitempty"`
	SecretRef string      `json:"secret_ref,omitempty"`
	Revision  int64       `json:"revision"`
	CreatedBy core.Actor  `json:"created_by"`
}
type channelInput struct {
	ID               string      `json:"id,omitempty"`
	ExpectedRevision int64       `json:"expected_revision,omitempty"`
	Name             string      `json:"name"`
	Kind             string      `json:"kind"`
	SMTP             *SMTPConfig `json:"smtp,omitempty"`
	ChatID           string      `json:"chat_id,omitempty"`
	Secret           string      `json:"secret,omitempty"`
}
type notificationJob struct {
	Actor     core.Actor `json:"actor"`
	ChannelID string     `json:"channel_id"`
	Subject   string     `json:"subject"`
	Content   string     `json:"content"`
}
type Delivery struct {
	ID        string    `json:"id"`
	ChannelID string    `json:"channel_id"`
	Timestamp time.Time `json:"timestamp"`
	Status    string    `json:"status"`
	ErrorCode string    `json:"error_code,omitempty"`
}

func (p *Provider) notificationResources(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var in struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
	}
	_ = json.Unmarshal(raw, &in)
	id := in.ID
	if id == "" {
		id = in.ChannelID
	}
	if id == "" {
		return []string{"host:local"}, nil
	}
	return []string{"notification:" + id}, nil
}
func (p *Provider) notificationBindings() []core.Binding {
	fields := map[string]any{"id": text(96), "expected_revision": integer(1, 1<<30), "name": text(160), "kind": enum("smtp", "telegram"), "chat_id": text(64), "secret": text(16384), "smtp": object(map[string]any{"host": text(253), "port": integer(1, 65535), "tls_mode": enum("implicit", "starttls"), "username": text(256), "from": text(320), "recipients": map[string]any{"type": "array", "items": text(320), "minItems": 1, "maxItems": 20}}, "host", "port", "tls_mode", "from", "recipients")}
	return []core.Binding{
		p.bind("notifications_channels_list", "List configured channels without returning credentials", "notifications:read", true, object(map[string]any{}), func(context.Context, core.Actor, json.RawMessage) ([]string, error) { return nil, nil }, p.listChannels),
		p.bind("notifications_channels_save", "Create/update a verified-TLS channel with encrypted resource-bound credentials", "notifications:write", false, object(fields, "name", "kind"), p.notificationResources, p.saveChannel),
		p.bind("notifications_channels_delete", "Delete a channel and its bound secret", "notifications:write", false, object(map[string]any{"id": text(96), "expected_revision": integer(1, 1<<30)}, "id", "expected_revision"), p.notificationResources, p.deleteChannel),
		p.bind("notifications_send", "Submit a real SMTP/Telegram delivery task; acceptance is distinct from final recipient delivery", "notifications:write", false, object(map[string]any{"channel_id": text(96), "subject": text(256), "content": text(32000)}, "channel_id", "subject", "content"), p.notificationResources, p.sendNotification),
		p.bind("notifications_deliveries", "Read bounded channel delivery outcomes without content or credentials", "notifications:read", true, object(map[string]any{"channel_id": text(96)}, "channel_id"), p.notificationResources, p.deliveries),
	}
}
func (p *Provider) loadChannel(ctx context.Context, id string) (Channel, error) {
	raw, rev, err := p.deps.State.GetVersioned(ctx, "notification_channel", id)
	if err != nil {
		return Channel{}, err
	}
	var out Channel
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	out.Revision = rev
	return out, nil
}
func (p *Provider) listChannels(ctx context.Context, a core.Actor, _ json.RawMessage) (core.ActionResult, error) {
	records, err := p.deps.State.List(ctx, "notification_channel")
	if err != nil {
		return core.ActionResult{}, err
	}
	out := []Channel{}
	for id := range records {
		if !p.deps.Authorize(a, "notifications:read", "notification:"+id) {
			continue
		}
		channel, err := p.loadChannel(ctx, id)
		if err != nil {
			return core.ActionResult{}, err
		}
		channel.SecretRef = ""
		out = append(out, channel)
	}
	return core.ActionResult{Data: out}, nil
}
func (p *Provider) saveChannel(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in channelInput
	if err := input(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	if in.Name == "" {
		return core.ActionResult{}, invalid("Channel name required")
	}
	channel := Channel{ID: in.ID, Name: in.Name, Kind: in.Kind, SMTP: in.SMTP, ChatID: in.ChatID, CreatedBy: a.Public()}
	oldSecret := ""
	var err error
	if in.Kind == "smtp" {
		if in.SMTP == nil || in.SMTP.Host == "" || strings.ContainsAny(in.SMTP.Host, " /\r\n\x00") || in.SMTP.Port < 1 || in.SMTP.Port > 65535 || (in.SMTP.TLSMode != "implicit" && in.SMTP.TLSMode != "starttls") || len(in.SMTP.Recipients) < 1 || len(in.SMTP.Recipients) > 20 {
			return core.ActionResult{}, invalid("Invalid verified-TLS SMTP configuration")
		}
		for _, address := range append([]string{in.SMTP.From}, in.SMTP.Recipients...) {
			if strings.ContainsAny(address, "\r\n") {
				return core.ActionResult{}, invalid("Invalid mail address")
			}
			if _, err = mail.ParseAddress(address); err != nil {
				return core.ActionResult{}, invalid("Invalid mail address")
			}
		}
		if in.ChatID != "" {
			return core.ActionResult{}, invalid("Telegram fields cannot be mixed into SMTP")
		}
	} else if in.Kind == "telegram" {
		if in.SMTP != nil {
			return core.ActionResult{}, invalid("SMTP fields cannot be mixed into Telegram")
		}
		if _, err = strconv.ParseInt(in.ChatID, 10, 64); err != nil {
			return core.ActionResult{}, invalid("Telegram chat ID must be numeric")
		}
	} else {
		return core.ActionResult{}, invalid("Unsupported notification channel")
	}
	if in.ID == "" {
		if in.ExpectedRevision != 0 {
			return core.ActionResult{}, invalid("New channels have no expected revision")
		}
		channel.ID, err = newID("ntf_")
		if err != nil {
			return core.ActionResult{}, err
		}
		if !p.deps.Authorize(a, "notifications:write", "notification:"+channel.ID) {
			return core.ActionResult{}, denied()
		}
	} else {
		old, err := p.loadChannel(ctx, in.ID)
		if err != nil {
			return core.ActionResult{}, err
		}
		if old.Revision != in.ExpectedRevision {
			return core.ActionResult{}, conflict("Channel revision changed")
		}
		channel.SecretRef = old.SecretRef
		oldSecret = old.SecretRef
		if channel.Kind != old.Kind && in.Secret == "" {
			return core.ActionResult{}, invalid("Changing channel type requires new credentials")
		}
		channel.CreatedBy = old.CreatedBy
	}
	if in.Secret != "" {
		channel.SecretRef, err = p.deps.Secrets.Put(ctx, a, "notification:"+channel.ID, "notification_credentials", []byte(in.Secret))
		if err != nil {
			return core.ActionResult{}, err
		}
	}
	if channel.Kind == "telegram" && channel.SecretRef == "" {
		return core.ActionResult{}, invalid("Telegram credentials required")
	}
	if channel.SMTP != nil && channel.SMTP.Username != "" && channel.SecretRef == "" {
		return core.ActionResult{}, invalid("SMTP credentials required")
	}
	channel.Revision = in.ExpectedRevision + 1
	value, _ := json.Marshal(channel)
	var rev int64
	err = p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
		if in.ID == "" {
			records, err := store.List(ctx, "notification_channel")
			if err != nil {
				return err
			}
			if len(records) >= 256 {
				return invalid("Notification channel limit (256) reached")
			}
		}
		var swapErr error
		rev, swapErr = store.CompareAndSwap(ctx, "notification_channel", channel.ID, in.ExpectedRevision, value)
		return swapErr
	})
	if err != nil && in.Secret != "" && channel.SecretRef != "" {
		_ = p.deps.Secrets.Delete(context.WithoutCancel(ctx), a, "notification:"+channel.ID, channel.SecretRef)
	}
	if err == nil && in.Secret != "" && oldSecret != "" {
		_ = p.deps.Secrets.Delete(context.WithoutCancel(ctx), a, "notification:"+channel.ID, oldSecret)
	}
	channel.Revision = rev
	channel.SecretRef = ""
	resource := "notification:" + channel.ID
	if in.ID == "" {
		resource = "host:local"
	}
	return core.ActionResult{Data: channel, Resources: []string{resource}, AuditSummary: map[string]any{"id": channel.ID, "kind": channel.Kind}}, err
}
func (p *Provider) deleteChannel(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in struct {
		ID               string `json:"id"`
		ExpectedRevision int64  `json:"expected_revision"`
	}
	if err := input(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	channel, err := p.loadChannel(ctx, in.ID)
	if err != nil {
		return core.ActionResult{}, err
	}
	if channel.Revision != in.ExpectedRevision {
		return core.ActionResult{}, conflict("Channel revision changed")
	}
	err = p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
		_, revision, err := store.GetVersioned(ctx, "notification_channel", in.ID)
		if err != nil {
			return err
		}
		if revision != in.ExpectedRevision {
			return conflict("Channel revision changed")
		}
		if err = store.Delete(ctx, "notification_channel", in.ID); err != nil {
			return err
		}
		return store.Delete(ctx, "notification_deliveries", in.ID)
	})
	if err == nil && channel.SecretRef != "" {
		err = p.deps.Secrets.Delete(ctx, a, "notification:"+in.ID, channel.SecretRef)
	}
	return core.ActionResult{Data: map[string]any{"deleted": err == nil}}, err
}
func (p *Provider) sendNotification(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in struct {
		ChannelID string `json:"channel_id"`
		Subject   string `json:"subject"`
		Content   string `json:"content"`
	}
	if err := input(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	if strings.ContainsAny(in.Subject, "\r\n") {
		return core.ActionResult{}, invalid("Subject cannot contain a newline")
	}
	if _, err := p.loadChannel(ctx, in.ChannelID); err != nil {
		return core.ActionResult{}, err
	}
	job := notificationJob{Actor: a.Public(), ChannelID: in.ChannelID, Subject: in.Subject, Content: in.Content}
	payload, _ := json.Marshal(job)
	result, err := p.deps.Tasks.Submit(ctx, a, "systemops_notification_send", payload, []string{"notification:" + in.ChannelID})
	id, _ := result["id"].(string)
	if id != "" {
		result["task_id"] = id
	}
	return core.ActionResult{Data: result, TaskID: id, Resources: []string{"notification:" + in.ChannelID}}, err
}
func (p *Provider) deliveries(ctx context.Context, _ core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in struct {
		ChannelID string `json:"channel_id"`
	}
	if err := input(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	out, err := channelDeliveries(ctx, p.deps.State, in.ChannelID)
	if err != nil {
		return core.ActionResult{}, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	if len(out) > 100 {
		out = out[:100]
	}
	return core.ActionResult{Data: out}, nil
}

func (p *Provider) deliver(ctx context.Context, job notificationJob) (Delivery, error) {
	actor, err := p.deps.ResolveActor(ctx, job.Actor)
	if err != nil {
		return Delivery{}, err
	}
	resource := "notification:" + job.ChannelID
	if !p.deps.Authorize(actor, "notifications:write", resource) {
		return Delivery{}, denied()
	}
	channel, err := p.loadChannel(ctx, job.ChannelID)
	if err != nil {
		return Delivery{}, err
	}
	var secret []byte
	if channel.SecretRef != "" {
		secret, err = p.deps.Secrets.Get(ctx, actor, resource, channel.SecretRef)
		if err != nil {
			return Delivery{}, err
		}
	}
	switch channel.Kind {
	case "smtp":
		err = p.smtpSend(ctx, *channel.SMTP, string(secret), job.Subject, job.Content)
	case "telegram":
		err = p.telegramSend(ctx, channel.ChatID, string(secret), job.Subject+"\n\n"+job.Content)
	default:
		err = errors.New("unsupported channel")
	}
	id, idErr := newID("delivery_")
	if idErr != nil {
		return Delivery{}, idErr
	}
	delivery := Delivery{ID: id, ChannelID: channel.ID, Timestamp: time.Now().UTC(), Status: "accepted"}
	if err != nil {
		delivery.Status = "failed"
		delivery.ErrorCode = "delivery_failed"
	}
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if saveErr := p.deps.State.Transaction(storeCtx, func(store core.MetadataStore) error {
		deliveries, err := channelDeliveries(storeCtx, store, channel.ID)
		if err != nil {
			return err
		}
		deliveries = append(deliveries, delivery)
		if len(deliveries) > 100 {
			deliveries = deliveries[len(deliveries)-100:]
		}
		value, _ := json.Marshal(deliveries)
		return store.Save(storeCtx, "notification_deliveries", channel.ID, value)
	}); saveErr != nil {
		return delivery, saveErr
	}
	if err != nil {
		return delivery, errors.New("verified notification delivery failed")
	}
	return delivery, nil
}
func channelDeliveries(ctx context.Context, store core.MetadataStore, id string) ([]Delivery, error) {
	value, err := store.Get(ctx, "notification_deliveries", id)
	if core.ErrorStatus(err) == 404 {
		return []Delivery{}, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Delivery
	err = json.Unmarshal(value, &out)
	return out, err
}
func (p *Provider) smtpSend(ctx context.Context, config SMTPConfig, password, subject, content string) error {
	address := net.JoinHostPort(config.Host, strconv.Itoa(config.Port))
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: config.Host, RootCAs: p.options.TLSRoots}
	var connection net.Conn
	var err error
	if config.TLSMode == "implicit" {
		connection, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", address)
	} else {
		connection, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return err
	}
	defer connection.Close()
	deadline := time.Now().Add(30 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	_ = connection.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	client, err := smtp.NewClient(connection, config.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	if config.TLSMode == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP server lacks required STARTTLS")
		}
		if err = client.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if config.Username != "" {
		if err = client.Auth(smtp.PlainAuth("", config.Username, password, config.Host)); err != nil {
			return err
		}
	}
	from, _ := mail.ParseAddress(config.From)
	if err = client.Mail(from.Address); err != nil {
		return err
	}
	recipients := []string{}
	for _, value := range config.Recipients {
		to, _ := mail.ParseAddress(value)
		recipients = append(recipients, to.String())
		if err = client.Rcpt(to.Address); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	var message bytes.Buffer
	fmt.Fprintf(&message, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", from.String(), strings.Join(recipients, ", "), mime.QEncoding.Encode("UTF-8", subject), time.Now().Format(time.RFC1123Z))
	encoder := quotedprintable.NewWriter(&message)
	_, err = encoder.Write([]byte(content))
	if err == nil {
		err = encoder.Close()
	}
	if err == nil {
		_, err = writer.Write(message.Bytes())
	}
	closeErr := writer.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return client.Quit()
}
func (p *Provider) telegramSend(ctx context.Context, chatID, token, content string) error {
	if utf8.RuneCountInString(content) > 4096 {
		return errors.New("Telegram messages are limited to 4096 characters")
	}
	if token == "" || strings.ContainsAny(token, "/\r\n?#") {
		return errors.New("invalid Telegram credential")
	}
	endpoint := p.options.TelegramEndpoint
	if endpoint == "" {
		endpoint = "https://api.telegram.org"
	}
	base, err := url.Parse(endpoint)
	if err != nil || base.Scheme != "https" || base.User != nil {
		return errors.New("trusted Telegram endpoint requires HTTPS")
	}
	body, _ := json.Marshal(map[string]string{"chat_id": chatID, "text": content})
	request, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(endpoint, "/")+"/bot"+token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.options.TLSRoots}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("notification redirects are not permitted")
	}}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Telegram request failed")
	}
	defer response.Body.Close()
	var result struct {
		OK bool `json:"ok"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil || response.StatusCode != 200 || !result.OK {
		return errors.New("Telegram did not accept the message")
	}
	return nil
}

var JobOperationNames = []string{"systemops_notification_send", "systemops_host_apply", "systemops_host_rollback", "systemops_script_run"}

type hostJob struct {
	Actor     core.Actor  `json:"actor"`
	Operation string      `json:"operation"`
	Request   hostRequest `json:"request"`
}

func (p *Provider) Validate(request agent.Request) error {
	switch request.Operation {
	case "systemops_script_run":
		var job scriptJob
		if err := strictJSON(request.Input, &job); err != nil {
			return err
		}
		if job.Actor.ID == "" || !identifier.MatchString(job.ID) || job.ExpectedRevision < 1 {
			return agent.ErrInvalidInput
		}
		return nil
	case "systemops_notification_send":
		var job notificationJob
		if err := strictJSON(request.Input, &job); err != nil {
			return err
		}
		if job.Actor.ID == "" || !identifier.MatchString(job.ChannelID) || len(job.Content) > 32000 || len(job.Subject) > 256 || strings.ContainsAny(job.Subject, "\r\n") {
			return agent.ErrInvalidInput
		}
		return nil
	case "systemops_host_apply", "systemops_host_rollback":
		var job hostJob
		if err := strictJSON(request.Input, &job); err != nil {
			return err
		}
		if job.Actor.ID == "" || job.Request.ActorID != job.Actor.ID || !identifier.MatchString(job.Request.ID) || (job.Operation != "system_change_apply" && job.Operation != "system_change_rollback") {
			return agent.ErrInvalidInput
		}
		return nil
	}
	return agent.ErrUnauthorized
}
func (p *Provider) Run(ctx context.Context, request agent.Request) (result agent.Result, err error) {
	result.Operation = request.Operation
	result.StartedAt = time.Now().UTC()
	result.ExitCode = -1
	defer func() {
		result.FinishedAt = time.Now().UTC()
		if err == nil {
			result.ExitCode = 0
		}
	}()
	if err = p.Validate(request); err != nil {
		return result, err
	}
	if request.Operation == "systemops_script_run" {
		return p.executeScript(ctx, request)
	}
	var data any
	if request.Operation == "systemops_notification_send" {
		var job notificationJob
		_ = json.Unmarshal(request.Input, &job)
		data, err = p.deliver(ctx, job)
	} else {
		var job hostJob
		_ = json.Unmarshal(request.Input, &job)
		var actor core.Actor
		actor, err = p.deps.ResolveActor(ctx, job.Actor)
		if err != nil {
			return result, err
		}
		if !p.deps.Authorize(actor, "system:write", "host:local") {
			return result, denied()
		}
		if p.options.Agent == nil {
			return result, unavailable("Privileged Agent is not configured")
		}
		raw, _ := json.Marshal(job.Request)
		return p.options.Agent.Run(ctx, agent.Request{Operation: job.Operation, Input: raw, Output: request.Output})
	}
	if err == nil {
		raw, _ := json.Marshal(data)
		result.Stdout = string(raw)
	}
	return result, err
}

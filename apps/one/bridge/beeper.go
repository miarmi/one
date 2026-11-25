package main

// Everything that knows what Beeper Desktop's local API looks like is in this
// one file. The shapes are those of Beeper's own documentation
// (developers.beeper.com/desktop-api-reference, read 2026-11-20): base
// http://localhost:23373 on this Mac, bearer token, lists that page with
// `cursor` and `direction` and say `hasMore`. What the documentation does not
// say is listed in the README under "Not yet checked on a real Mac".

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

type beeperClient struct {
	base  string
	token string
	hc    *http.Client
}

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		// An address that answers with a redirect is never followed: the
		// bridge talks to the two addresses it was given and nothing else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func newBeeper(base, token string) *beeperClient {
	return &beeperClient{base: base, token: token, hc: newHTTPClient(30 * time.Second)}
}

// errStatus is an answer that was not 2xx. It carries only the number.
type errStatus int

func (e errStatus) Error() string { return fmt.Sprintf("answered %d", int(e)) }

func (c *beeperClient) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, errors.New("bad request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, errors.New("Beeper is not reachable")
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, errStatus(resp.StatusCode)
	}
	return resp, nil
}

func (c *beeperClient) json(ctx context.Context, method, path string, in, out any) error {
	var rd io.Reader
	ct := ""
	if in != nil {
		j, _ := json.Marshal(in)
		rd, ct = bytes.NewReader(j), "application/json"
	}
	resp, err := c.do(ctx, method, path, rd, ct)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out); err != nil {
		return errors.New("Beeper's answer could not be read")
	}
	return nil
}

// --- what Beeper tells us ---

// bAccount is a login of a network in Beeper. Status is one of connected,
// connecting, backfilling, connection_required, reconnect_required,
// attention_required, disconnected and disabled.
type bAccount struct {
	AccountID  string `json:"accountID"`
	Network    string `json:"network"`
	Status     string `json:"status"`
	StatusText string `json:"statusText"`
}

type bPerson struct {
	ID       string `json:"id"`
	FullName string `json:"fullName"`
	IsSelf   bool   `json:"isSelf"`
}

type bChat struct {
	ID           string `json:"id"`
	AccountID    string `json:"accountID"`
	Network      string `json:"network"`
	Title        string `json:"title"`
	Type         string `json:"type"` // single or group
	IsMuted      bool   `json:"isMuted"`
	LastActivity string `json:"lastActivity"`
	Participants struct {
		Items []bPerson `json:"items"`
	} `json:"participants"`
}

// bAttachment: `size` is an object {width, height} (kept raw: it is not used),
// the number of bytes is `fileSize`, `id` is usually an mxc:// address and
// `srcURL` a URL or a local file path.
type bAttachment struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	MimeType string          `json:"mimeType"`
	FileName string          `json:"fileName"`
	FileSize int64           `json:"fileSize"`
	SrcURL   string          `json:"srcURL"`
	Size     json.RawMessage `json:"size"`
}

type bReaction struct {
	Key           string `json:"reactionKey"`
	ParticipantID string `json:"participantID"`
}

type bSendStatus struct {
	Status  string `json:"status"` // SUCCESS, PENDING, FAIL_RETRIABLE or FAIL_PERMANENT
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

type bMessage struct {
	ID              string        `json:"id"`
	ChatID          string        `json:"chatID"`
	SenderID        string        `json:"senderID"`
	SenderName      string        `json:"senderName"`
	SortKey         string        `json:"sortKey"`
	Timestamp       string        `json:"timestamp"`
	Text            string        `json:"text"`
	Type            string        `json:"type"`
	IsSender        bool          `json:"isSender"`
	IsUnread        bool          `json:"isUnread"`
	IsDeleted       bool          `json:"isDeleted"`
	IsHidden        bool          `json:"isHidden"`
	EditedTimestamp string        `json:"editedTimestamp"`
	LinkedMessageID string        `json:"linkedMessageID"`
	Attachments     []bAttachment `json:"attachments"`
	Reactions       []bReaction   `json:"reactions"`
	SendStatus      *bSendStatus  `json:"sendStatus"`
}

// bPage is how Beeper pages a list: the items, whether there are more in the
// direction asked, and the cursors of the newest and the oldest item.
type bPage[T any] struct {
	Items        []T    `json:"items"`
	HasMore      bool   `json:"hasMore"`
	NewestCursor string `json:"newestCursor"`
	OldestCursor string `json:"oldestCursor"`
}

func pageQuery(cursor, direction string, extra url.Values) string {
	q := url.Values{}
	for k, v := range extra {
		q[k] = v
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if direction != "" {
		q.Set("direction", direction)
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// Accounts lists the logins. The answer is a bare array.
func (c *beeperClient) Accounts(ctx context.Context) ([]bAccount, error) {
	var arr []bAccount
	if err := c.json(ctx, "GET", "/v1/accounts", nil, &arr); err != nil {
		return nil, err
	}
	return arr, nil
}

// Chats is one page of the chats, newest activity first.
func (c *beeperClient) Chats(ctx context.Context, cursor, direction string) (bPage[bChat], error) {
	var out bPage[bChat]
	err := c.json(ctx, "GET", "/v1/chats"+pageQuery(cursor, direction, url.Values{"limit": {"200"}}), nil, &out)
	return out, err
}

// Messages is one page of the messages of a chat, oldest first within the
// page. Only cursor and direction are documented; there is no limit.
func (c *beeperClient) Messages(ctx context.Context, chatID, cursor, direction string) (bPage[bMessage], error) {
	var out bPage[bMessage]
	err := c.json(ctx, "GET", "/v1/chats/"+url.PathEscape(chatID)+"/messages"+pageQuery(cursor, direction, nil), nil, &out)
	return out, err
}

// Message reads one message.
func (c *beeperClient) Message(ctx context.Context, chatID, messageID string) (bMessage, error) {
	var out bMessage
	err := c.json(ctx, "GET", "/v1/chats/"+url.PathEscape(chatID)+"/messages/"+url.PathEscape(messageID), nil, &out)
	return out, err
}

// sendAttachment is the one attachment a message can carry.
type sendAttachment struct {
	UploadID string `json:"uploadID"`
	FileName string `json:"fileName,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Type     string `json:"type,omitempty"` // image, video, audio, file, gif, voice-note or sticker
}

// attachmentType says which kind of attachment a MIME type is for Beeper.
func attachmentType(mime string) string {
	m := strings.ToLower(mime)
	switch {
	case m == "image/gif":
		return "gif"
	case strings.HasPrefix(m, "image/"):
		return "image"
	case strings.HasPrefix(m, "video/"):
		return "video"
	case strings.HasPrefix(m, "audio/"):
		return "audio"
	}
	return "file"
}

// Send posts a message in a chat: a text, or one attachment (after its text,
// as a message of its own). pending is the provisional id; chat is the chat it
// really went to (a merged chat can differ from the one asked: use it for any
// follow-up). definite is true when Beeper answered, so the message did not go;
// false when the answer is missing and the message may have gone anyway.
func (c *beeperClient) Send(ctx context.Context, chatID, text string, att *sendAttachment) (pending, chat string, definite bool, err error) {
	body := map[string]any{}
	if text != "" {
		body["text"] = text
	}
	if att != nil {
		body["attachment"] = att
	}
	j, _ := json.Marshal(body)
	resp, err := c.do(ctx, "POST", "/v1/chats/"+url.PathEscape(chatID)+"/messages", bytes.NewReader(j), "application/json")
	if err != nil {
		var st errStatus
		return "", "", errors.As(err, &st), err
	}
	defer resp.Body.Close()
	var out struct {
		PendingMessageID string `json:"pendingMessageID"`
		ChatID           string `json:"chatID"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", "", false, errors.New("Beeper's answer could not be read")
	}
	if out.ChatID == "" {
		out.ChatID = chatID
	}
	return out.PendingMessageID, out.ChatID, true, nil
}

// Download asks Beeper to fetch a media address (mxc:// or localmxc://) and
// answers with the local file it made. The bridge runs on the same Mac, so it
// reads that file.
func (c *beeperClient) Download(ctx context.Context, mediaURL string) (string, error) {
	var out struct {
		SrcURL string `json:"srcURL"`
		Error  string `json:"error"`
	}
	if err := c.json(ctx, "POST", "/v1/assets/download", map[string]string{"url": mediaURL}, &out); err != nil {
		return "", err
	}
	if out.Error != "" || out.SrcURL == "" {
		return "", errors.New("Beeper could not fetch the file")
	}
	return out.SrcURL, nil
}

// Upload hands Beeper a file to send, as multipart form data, streamed: the
// file is never held in memory as a whole. It answers with the uploadID.
func (c *beeperClient) Upload(ctx context.Context, name, mime string, r io.Reader) (string, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			if name != "" {
				if err := mw.WriteField("fileName", name); err != nil {
					return err
				}
			}
			if mime != "" {
				if err := mw.WriteField("mimeType", mime); err != nil {
					return err
				}
			}
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, quoteName(name)))
			h.Set("Content-Type", "application/octet-stream")
			fw, err := mw.CreatePart(h)
			if err != nil {
				return err
			}
			if _, err := io.Copy(fw, r); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	resp, err := c.do(ctx, "POST", "/v1/assets/upload", pr, mw.FormDataContentType())
	if err != nil {
		pr.CloseWithError(err)
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		UploadID string `json:"uploadID"`
		Error    string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", errors.New("Beeper's answer could not be read")
	}
	if out.Error != "" || out.UploadID == "" {
		return "", errors.New("Beeper did not take the file")
	}
	return out.UploadID, nil
}

func quoteName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, s)
	if s == "" {
		s = "file"
	}
	return s
}

// networkName turns Beeper's account into the short lower-case name ONE uses.
func networkName(a bAccount) string {
	return shortNetwork(a.Network, a.AccountID)
}

func shortNetwork(network, fallback string) string {
	n := strings.ToLower(network)
	if n == "" {
		n = strings.ToLower(fallback)
	}
	var b strings.Builder
	for _, r := range n {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	switch {
	case strings.Contains(s, "messenger"), strings.HasPrefix(s, "facebook"):
		return "messenger"
	case strings.HasPrefix(s, "instagram"):
		return "instagram"
	case strings.HasPrefix(s, "whatsapp"):
		return "whatsapp"
	case strings.HasPrefix(s, "imessage"):
		return "imessage"
	}
	if len(s) < 2 {
		return "other"
	}
	if len(s) > 20 {
		s = s[:20]
	}
	return s
}

// needsLogin says whether a status of an account means the owner has to log in again.
func needsLogin(status string) bool {
	switch status {
	case "connection_required", "reconnect_required", "attention_required", "disconnected":
		return true
	}
	return false
}

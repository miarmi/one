package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/textproto"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// Replies to mail (SPEC 13). An agent proposes the text; the server works out
// where it goes; only the owner, on the signed-in page, can send it, and what goes
// out is the text in the box at the moment he presses Approve. Guards at work:
// Blocked targets, Same thread, Exact text and Send once. Chat replies are not
// part of this slice.

const (
	replyGmail   = "reply_gmail"
	replyOutlook = "reply_outlook"
	replyChat    = repo.ChatReply // a chat reply, sent by the bridge on the Mac

	maxReplyText   = 10000
	maxRecipients  = 10 // a reply to more people than this is not proposed
	keyBlockedList = "blocked_targets"
)

func isReplyAction(action string) bool {
	return action == replyGmail || action == replyOutlook || action == replyChat
}

// replyChannel says in words where the reply goes.
func replyChannel(action string) string {
	if action == replyOutlook {
		return "reply to the Office mail"
	}
	if action == replyChat {
		return "reply in chat"
	}
	return "reply to the mail"
}

// chatChannel says where a chat reply goes: "reply in Messenger".
func chatChannel(network string) string { return "reply in " + sourceLabel(network) }

// replyBoxLabel is the label over the box (SPEC 13, Exact wording).
func replyBoxLabel(action string) string { return "Will be sent · " + replyChannel(action) }

// replyChipLabel is what the chip on a closed row says.
func replyChipLabel(action string) string {
	return strings.ToUpper(replyChannel(action)[:1]) + replyChannel(action)[1:]
}

// --- Blocked targets: the list is data ---

// defaultBlockedTargets is where the list starts: banks, payment services,
// the data box and senders of sign-in codes, as domains (which include their
// subdomains) or exact addresses. The owner edits it in Settings.
var defaultBlockedTargets = []string{
	// banks (Czech and Slovak)
	"unicreditbank.cz", "unicreditgroup.eu", "csob.cz", "kb.cz", "moneta.cz", "fio.cz", "airbank.cz", "rb.cz", "raiffeisenbank.cz",
	"csas.cz", "ceskasporitelna.cz", "mbank.cz", "creditas.cz", "equabank.cz", "tatrabanka.sk", "slsp.sk", "vub.sk", "postovabanka.sk",
	// payment services
	"paypal.com", "stripe.com", "wise.com", "revolut.com", "gopay.cz", "gopay.com", "gpwebpay.cz", "comgate.cz", "payu.com", "payu.cz",
	"adyen.com", "klarna.com", "skrill.com", "thepay.cz", "barion.com", "worldpay.com",
	// the data box (datová schránka)
	"mojedatovaschranka.cz", "datoveschranky.info", "isds.cz",
	// senders of sign-in codes
	"accounts.google.com", "accountprotection.microsoft.com", "account.microsoft.com", "id.apple.com", "appleid.com", "facebookmail.com",
}

// blockedTargets is the list in force: The owner's saved list, or the starting one.
func (a *app) blockedTargets(ctx context.Context, q repo.Q) []string {
	if q.SettingAt(ctx, keyBlockedList) == 0 {
		return defaultBlockedTargets
	}
	return parseTargetList(q.Setting(ctx, keyBlockedList))
}

// parseTargetList reads one entry per line, lower case; "-" alone is an empty list.
func parseTargetList(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		l = strings.ToLower(strings.TrimSpace(l))
		if l != "" && l != "-" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

var targetEntryRE = regexp.MustCompile(`^(?:[a-z0-9._+%-]+@)?[a-z0-9-]+(?:\.[a-z0-9-]+)+$`)

// blockedBy says whether an address is on the list: an entry with an @ is that
// address, any other entry is a domain with all its subdomains.
func blockedBy(list []string, addr string) bool {
	addr = strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return false
	}
	domain := addr[at+1:]
	for _, e := range list {
		if strings.Contains(e, "@") {
			if e == addr {
				return true
			}
			continue
		}
		if domain == e || strings.HasSuffix(domain, "."+e) {
			return true
		}
	}
	return false
}

// noReply says whether an address is one nobody reads: a reply to it would only
// bounce, or be read by a machine.
func noReply(addr string) bool {
	local, _, _ := strings.Cut(strings.ToLower(addr), "@")
	local = strings.NewReplacer("-", "", "_", "", ".", "", "+", "").Replace(local)
	for _, p := range []string{"noreply", "donotreply", "neodpovidejte", "mailerdaemon", "postmaster", "bounce"} {
		if strings.HasPrefix(local, p) {
			return true
		}
	}
	return false
}

// --- who it goes to ---

type replyTo struct {
	To []string `json:"to"`
	Cc []string `json:"cc"`
	// A chat reply goes into one chat: its network, its id and its name.
	Network  string `json:"network,omitempty"`
	ChatID   string `json:"chat_id,omitempty"`
	ChatName string `json:"chat_name,omitempty"`
	Group    bool   `json:"group,omitempty"`
}

func (r replyTo) all() []string { return append(slices.Clone(r.To), r.Cc...) }

func parseRecipients(s string) replyTo {
	var r replyTo
	json.Unmarshal([]byte(s), &r)
	return r
}

func splitAddrs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// plainAddress is an address a mail can carry here: one plain address in ASCII.
func plainAddress(s string) (string, bool) {
	a, ok := cleanAddress(s)
	if !ok || !isASCII(a) {
		return "", false
	}
	return strings.ToLower(a), true
}

// ownAddresses are the owner's own addresses, which are never a recipient: the
// connected ones, those remembered from when they were connected (a sign-in
// that ran out takes the secret away, not the address), and those his sent
// mail came from.
func (a *app) ownAddresses(ctx context.Context, q repo.Q) []string {
	var own []string
	if s, ok := a.gmailSecret(); ok {
		own = append(own, strings.ToLower(s.Address))
	}
	var g graphSecret
	if readSecret(a.secretsDir, graphSecretFile, &g) && g.Address != "" {
		own = append(own, strings.ToLower(g.Address))
	}
	for _, src := range []string{srcGmail, srcGraph} {
		if v := strings.ToLower(strings.TrimSpace(q.Setting(ctx, "own_address."+src))); v != "" {
			own = append(own, v)
		}
	}
	return append(own, q.OwnAddresses(ctx)...)
}

// canonAddr is an address as the mailbox behind it sees it: a +tag is not
// part of the name, and Gmail also ignores dots and answers on googlemail.com.
func canonAddr(ad string) string {
	ad = strings.ToLower(strings.TrimSpace(ad))
	i := strings.LastIndexByte(ad, '@')
	if i <= 0 {
		return ad
	}
	local, domain := ad[:i], ad[i+1:]
	if j := strings.IndexByte(local, '+'); j > 0 {
		local = local[:j]
	}
	if domain == "googlemail.com" {
		domain = "gmail.com"
	}
	if domain == "gmail.com" {
		local = strings.ReplaceAll(local, ".", "")
	}
	return local + "@" + domain
}

// isOwn says whether an address is one of the owner's own, also when it is
// written with a +tag or as another spelling of his Gmail address.
func isOwn(own []string, ad string) bool {
	c := canonAddr(ad)
	for _, o := range own {
		if canonAddr(o) == c {
			return true
		}
	}
	return false
}

// rememberOwn keeps the connected addresses in the data, so that they stay
// The owner's own after a sign-in has run out.
func (a *app) rememberOwn(ctx context.Context, tx *repo.Tx) error {
	put := func(src, addr string) error {
		addr = strings.ToLower(strings.TrimSpace(addr))
		if addr == "" || tx.Setting(ctx, "own_address."+src) == addr {
			return nil
		}
		return tx.SetSettingBy(ctx, "own_address."+src, addr, "system", a.now())
	}
	if s, ok := a.gmailSecret(); ok {
		if err := put(srcGmail, s.Address); err != nil {
			return err
		}
	}
	var g graphSecret
	if readSecret(a.secretsDir, graphSecretFile, &g) {
		return put(srcGraph, g.Address)
	}
	return nil
}

// originalHeaders reads the headers of a message from the original the pool
// keeps (empty when it cannot be read).
func (a *app) originalHeaders(ctx context.Context, q repo.Q, messageID int64) textproto.MIMEHeader {
	sha, err := q.MessageBlob(ctx, messageID)
	if err != nil || len(sha) < 3 {
		return nil
	}
	f, err := a.imp.core.pool.open(relPath(sha))
	if err != nil {
		return nil
	}
	defer f.Close()
	m, err := mail.ReadMessage(bufio.NewReaderSize(f, 64<<10))
	if err != nil {
		return nil
	}
	return textproto.MIMEHeader(m.Header)
}

// replyPlan is what a reply to an item comes to, all of it worked out by the
// server from the thread.
type replyPlan struct {
	Target     repo.Message
	Action     string
	Recipients replyTo
	References []string
}

// planReply works out the message answered and the people it goes to, or says
// why there can be no reply: a refusal by a guard or a plain conflict.
func (a *app) planReply(ctx context.Context, q repo.Q, it repo.Item) (replyPlan, error) {
	if it.Stage == stageClosed || it.Stage == stageHistory {
		return replyPlan{}, conflict("This item is closed. Bring it back before a reply is proposed.")
	}
	newest, ok := q.NewestMessage(ctx, it.ID)
	if !ok {
		return replyPlan{}, conflict("This item has no message to answer.")
	}
	return a.planReplyTo(ctx, q, it, newest)
}

// planReplyTo is planReply for a given message of the item: a reply answers
// the newest message of a row, so on a row that holds mail and chat it goes to
// the channel of that message, and the guards (Blocked targets, Same thread)
// look at the thread it goes to, not at the row as a whole (SPEC 10). At the
// moment of sending, the message the proposal answered is planned again.
func (a *app) planReplyTo(ctx context.Context, q repo.Q, it repo.Item, newest repo.Message) (replyPlan, error) {
	var p replyPlan
	if it.Stage == stageClosed || it.Stage == stageHistory {
		return p, conflict("This item is closed. Bring it back before a reply is proposed.")
	}
	if newest.SentByMe {
		return p, refuse("Same thread", whySameOwn)
	}
	switch {
	case newest.Network == "mail" && newest.Source == srcGmail:
		p.Action = replyGmail
	case newest.Network == "mail" && newest.Source == srcGraph:
		p.Action = replyOutlook
	case newest.Network != "mail":
		return a.planChatReply(ctx, q, it, newest)
	default:
		return p, conflict("Only replies to mail or to a chat can be proposed here.")
	}
	p.Target = newest
	blocked := a.blockedTargets(ctx, q)
	check := a.guardOn(ctx, q, "blocked_targets")
	if check {
		senders, err := q.SendersOfThread(ctx, it.ID, newest.ThreadKey)
		if err != nil {
			return p, err
		}
		for _, s := range senders {
			if blockedBy(blocked, s.Address) {
				return p, refuse("Blocked targets", whyBlockedReply)
			}
		}
	}
	sender, ok := plainAddress(newest.Sender)
	if !ok {
		return p, conflict("The message has no sender address that a reply can go to.")
	}
	if noReply(sender) {
		return p, conflict("The sender does not take replies (a no-reply address).")
	}
	own := a.ownAddresses(ctx, q)
	if isOwn(own, sender) {
		return p, conflict("The message is from one of your own addresses; there is nobody to reply to.")
	}
	hdr := a.originalHeaders(ctx, q, newest.ID)
	to := []string{sender}
	var cc []string
	// A message that came through a mailing list is answered to its writer
	// alone; the list and the people on it are not written to.
	list := hdr.Get("List-Id") != "" || hdr.Get("List-Post") != ""
	if !list {
		add := func(dst *[]string, addrs []string) {
			for _, raw := range addrs {
				ad, ok := plainAddress(raw)
				if !ok || isOwn(own, ad) || slices.Contains(to, ad) || slices.Contains(cc, ad) {
					continue // an address that cannot be used is left out; the box shows who is left
				}
				*dst = append(*dst, ad)
			}
		}
		add(&to, splitAddrs(newest.To))
		add(&cc, splitAddrs(newest.Cc))
	}
	if len(to)+len(cc) > maxRecipients {
		return p, conflict(fmt.Sprintf("The message went to more than %d people; answer it in the mail program.", maxRecipients))
	}
	if check {
		for _, ad := range append(slices.Clone(to), cc...) {
			if blockedBy(blocked, ad) {
				return p, refuse("Blocked targets", whyBlockedReply)
			}
		}
	}
	p.Recipients = replyTo{To: to, Cc: cc}
	switch p.Action {
	case replyGmail:
		id := strings.TrimSpace(newest.HeaderID)
		if !msgIDRE.MatchString(id) {
			return p, refuse("Same thread", whySameNoID)
		}
		refs := msgIDsOf(strings.Join(hdr.Values("References"), " "))
		if len(refs) == 0 {
			refs = msgIDsOf(strings.Join(hdr.Values("In-Reply-To"), " "))
		}
		refs = append(refs, id)
		if len(refs) > 20 {
			refs = refs[len(refs)-20:]
		}
		p.References = refs
	case replyOutlook:
		if strings.TrimSpace(newest.SourceID) == "" {
			return p, refuse("Same thread", whySameNoRef)
		}
	}
	return p, nil
}

// verifyRecipients is Same thread at the moment of sending: everyone the
// reply goes to is already on the message that is answered, and none of them
// is the owner.
func verifyRecipients(target repo.Message, r replyTo, own []string) error {
	onThread := append(splitAddrs(target.To), splitAddrs(target.Cc)...)
	onThread = append(onThread, strings.ToLower(strings.TrimSpace(target.Sender)))
	if len(r.To) == 0 {
		return refuse("Same thread", whySameNoOne)
	}
	for _, ad := range r.all() {
		if !slices.Contains(onThread, ad) || isOwn(own, ad) {
			return refuse("Same thread", whySameRecipients)
		}
	}
	return nil
}

// --- text ---

// cleanReplyText checks the text of a reply and gives it the way it will be
// sent: valid UTF-8, no control characters but tabs and line breaks, line
// ends as \n. Nothing is trimmed: what is sent is what was in the box.
func cleanReplyText(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", bad("The text is not valid UTF-8.")
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' {
			return "", bad("The text holds a character that cannot be sent.")
		}
		// Characters that do not show or that turn the reading direction can
		// hide what a text says from the person approving it.
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) ||
			(r >= 0x2060 && r <= 0x2069) || r == 0xfeff || r == 0x2028 || r == 0x2029 || r == 0x00ad {
			return "", bad("The text holds a hidden character.")
		}
	}
	if strings.TrimSpace(s) == "" {
		return "", bad("The text is empty.")
	}
	if utf8.RuneCountInString(s) > maxReplyText {
		return "", bad(fmt.Sprintf("The text is longer than %d characters.", maxReplyText))
	}
	return s, nil
}

// --- proposing (agents) ---

// proposeReply stores the text of a reply an agent wants sent. The server
// works out where it goes; nothing is sent, and an agent has no way to send.
func (a *app) proposeReply(ctx context.Context, x actor, ref, text, reason string) (result, error) {
	var res result
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	text, err := cleanReplyText(text)
	if err != nil {
		return res, err
	}
	reason, err = checkField("reason", reason, maxReason, true)
	if err != nil {
		return res, err
	}
	now := a.now().Unix()
	var itemID int64
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		it, err := a.findItemW(ctx, tx.Q, ref)
		if err != nil {
			return err
		}
		itemID, res.Item = it.ID, it
		if err := a.rememberOwn(ctx, tx); err != nil {
			return err
		}
		plan, err := a.planReply(ctx, tx.Q, it)
		if err != nil {
			return err
		}
		if tx.ReplySentFor(ctx, plan.Target.ID, 0) {
			return refuse("Send once", whySendOnceAnswered)
		}
		waiting, err := tx.ProposalsOfItem(ctx, it.ID)
		if err != nil {
			return err
		}
		for _, p := range waiting {
			if p.Status == "pending" && isReplyAction(p.Action) && p.Text == text && p.TargetID == plan.Target.ID {
				res.Proposal, res.Same = p.ID, true
				return nil
			}
		}
		// A new reply takes the place of one still waiting: only one can be sent.
		for _, p := range waiting {
			if p.Status == "pending" && isReplyAction(p.Action) {
				if _, err := tx.DecideProposal(ctx, p.ID, "withdrawn", "system", "replaced by a newer proposal", now); err != nil {
					return err
				}
				if err := tx.InsertEvent(ctx, now, it.ID, "system", "proposal_withdrawn", map[string]any{"proposal": p.ID, "why": "replaced by a newer proposal"}); err != nil {
					return err
				}
			}
		}
		if tx.PendingCount(ctx, it.ID) >= maxPending {
			return conflict("This item already has proposals waiting for the owner.")
		}
		rcpt, _ := json.Marshal(plan.Recipients)
		id, err := tx.InsertProposal(ctx, repo.Proposal{ItemID: it.ID, Action: plan.Action, Text: text, Reason: reason, By: x.id(), At: now,
			BaseStage: it.Stage, BaseMsgAt: tx.NewestMessageAt(ctx, it.ID), TargetID: plan.Target.ID, Recipient: string(rcpt)})
		if err != nil {
			return err
		}
		res.Proposal = id
		if err := tx.InsertEvent(ctx, now, it.ID, x.id(), "proposal_made", map[string]any{"proposal": id, "action": plan.Action, "reason": reason,
			"text": text, "to": plan.Recipients.To, "cc": plan.Recipients.Cc}); err != nil {
			return err
		}
		res.Revealed, err = a.answer(ctx, tx, x, it, now)
		return err
	})
	return res, a.logRefusal(ctx, x, itemID, err)
}

// --- approving (the owner, on the page) ---

// replyOutcome says what pressing Approve on a reply came to.
type replyOutcome struct {
	Item   repo.Item
	Status string // sent, failed, unknown, withdrawn, or "" when nothing was done
	Same   bool   // the proposal was not waiting: nothing was sent
}

// approveReply is Approve on a reply: it sends exactly the text it is given,
// the text in the box the owner pressed Approve on, once. Only the signed-in page
// calls it with the owner; any other actor is refused whatever the request says.
func (a *app) approveReply(ctx context.Context, x actor, proposalID int64, shown string) (replyOutcome, error) {
	var out replyOutcome
	if !x.owner {
		var itemID int64
		if p, ok := a.store.ProposalByID(ctx, proposalID); ok {
			itemID = p.ItemID
		}
		return out, a.logRefusal(ctx, x, itemID, refuse("Actor by channel", whyActorOwner))
	}
	text, err := cleanReplyText(shown)
	if err != nil {
		return out, err
	}
	now := a.now()
	var p repo.Proposal
	var claimed bool
	var secretAddr, ref string
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		var found bool
		p, found = tx.ProposalByID(ctx, proposalID)
		if !found {
			return errNoProposal
		}
		it, ok := tx.ItemByID(ctx, p.ItemID)
		if !ok {
			return errNoSuchItem()
		}
		out.Item = it
		if !isReplyAction(p.Action) {
			return bad("That proposal is not a reply.")
		}
		if p.Status != "pending" || p.SendState != "" {
			// A second Approve, a retry, a second device, or a proposal that was
			// dismissed, withdrawn or replaced: nothing is sent.
			out.Same, out.Status = true, p.Status
			if p.SendState != "" {
				out.Status = p.SendState
				return tx.InsertEvent(ctx, now.Unix(), it.ID, "system", "refused", map[string]any{"guard": "Send once", "why": whySendOnceSending})
			}
			return nil
		}
		withdraw := func(why string) error {
			out.Status = "withdrawn"
			if _, err := tx.DecideProposal(ctx, p.ID, "withdrawn", "system", why, now.Unix()); err != nil {
				return err
			}
			return tx.InsertEvent(ctx, now.Unix(), it.ID, "system", "proposal_withdrawn", map[string]any{"proposal": p.ID, "why": why})
		}
		// Same thread: the message answered must still be the newest of its own
		// thread (a message on the other thread of a row does not change it).
		target, ok := tx.MessageByID(ctx, p.TargetID)
		if !ok || target.ItemID != it.ID {
			return withdraw("it no longer fits")
		}
		newest, ok := tx.NewestMessageOfThread(ctx, it.ID, target.ThreadKey)
		if !ok || newest.ID != p.TargetID || newest.ReceivedAt > p.BaseMsgAt || newest.SentByMe || it.Stage == stageClosed || it.Stage == stageHistory {
			return withdraw("it no longer fits")
		}
		if tx.ReplySentFor(ctx, p.TargetID, p.ID) {
			return refuse("Send once", whySendOnceAnswered)
		}
		// The guards once more, at the moment of sending.
		plan, err := a.planReplyTo(ctx, tx.Q, it, newest)
		if err != nil {
			return err
		}
		stored := parseRecipients(p.Recipient)
		if p.Action == replyChat {
			// Same thread for a chat: the reply goes into the chat it was proposed on.
			if plan.Action != p.Action || plan.Recipients.Network != stored.Network || plan.Recipients.ChatID != stored.ChatID {
				return withdraw("the chat changed")
			}
		} else {
			if plan.Action != p.Action || !slices.Equal(plan.Recipients.To, stored.To) || !slices.Equal(plan.Recipients.Cc, stored.Cc) {
				return withdraw("the people it goes to changed")
			}
			if err := verifyRecipients(newest, stored, a.ownAddresses(ctx, tx.Q)); err != nil {
				return err
			}
		}
		if p.Action == replyGmail {
			if s, ok := a.gmailSecret(); ok {
				secretAddr = s.Address
				ref = newMessageID(s.Address)
			}
		}
		// The intent is written before anything leaves the app.
		if _, err := tx.DecideProposal(ctx, p.ID, "approved", "owner", "", now.Unix()); err != nil {
			return err
		}
		began, err := tx.BeginSend(ctx, p.ID, text, ref, now.Unix())
		if err != nil {
			return err
		}
		if !began {
			return refuse("Send once", whySendOnceSending)
		}
		claimed = true
		p.FinalText, p.SendRef = text, ref
		if p.Action == replyChat {
			// The intent waits for the Mac: it is handed to the bridge once, when it asks.
			if ok, err := tx.QueueSend(ctx, p.ID); err != nil || !ok {
				if err == nil {
					err = refuse("Send once", whySendOnceSending)
				}
				return err
			}
		}
		if err := tx.InsertEvent(ctx, now.Unix(), it.ID, "owner", "proposal_decided", map[string]any{"proposal": p.ID, "decision": "approved", "action": p.Action,
			"original": p.Text, "text": text, "edited": text != p.Text}); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, now.Unix(), it.ID, "owner", "send_intent", map[string]any{"proposal": p.ID, "action": p.Action, "to": stored.To, "cc": stored.Cc, "ref": ref})
	})
	if err != nil {
		var rf *refusal
		if errors.As(err, &rf) {
			a.logRefusal(ctx, x, p.ItemID, err)
		}
		return out, err
	}
	if !claimed {
		return out, nil
	}
	if p.Action == replyChat {
		out.Status = "queued"
		return out, nil
	}
	// The send itself. It is not stopped when the page's request is: a send
	// that is cut off in the middle would be one nobody can account for.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 70*time.Second)
	defer cancel()
	fail := a.deliverReply(sctx, p, secretAddr)
	out.Status = a.finishSend(context.WithoutCancel(ctx), p, fail)
	return out, nil
}

// deliverReply hands the reply to Gmail or to Microsoft.
func (a *app) deliverReply(ctx context.Context, p repo.Proposal, gmailAddr string) *sendFailure {
	rcpt := parseRecipients(p.Recipient)
	var target repo.Message
	var hdr textproto.MIMEHeader
	target, ok := a.store.MessageByID(ctx, p.TargetID)
	if !ok {
		return &sendFailure{Text: "The message to answer is no longer here, so nothing was sent."}
	}
	switch p.Action {
	case replyGmail:
		sec, ok := a.gmailSecret()
		if !ok || !strings.EqualFold(sec.Address, gmailAddr) {
			return &sendFailure{Text: "Gmail is not connected, so nothing was sent. Connect it on the Connectors page."}
		}
		hdr = a.originalHeaders(ctx, a.store.Q, target.ID)
		refs := msgIDsOf(strings.Join(hdr.Values("References"), " "))
		if len(refs) == 0 {
			refs = msgIDsOf(strings.Join(hdr.Values("In-Reply-To"), " "))
		}
		refs = append(refs, strings.TrimSpace(target.HeaderID))
		if len(refs) > 20 {
			refs = refs[len(refs)-20:]
		}
		msg := buildReplyMail(replyMail{From: sec.Address, To: rcpt.To, Cc: rcpt.Cc, Subject: target.Subject, InReplyTo: strings.TrimSpace(target.HeaderID),
			References: refs, Body: p.FinalText, Now: a.now(), MessageID: p.SendRef})
		return smtpDeliver(a.gmail, sec, rcpt.all(), msg)
	case replyOutlook:
		return a.graphReply(ctx, target.SourceID, p.FinalText, rcpt.To, rcpt.Cc)
	}
	return &sendFailure{Text: "That kind of reply cannot be sent."}
}

// finishSend writes how a send ended and moves the item: a reply that went
// out takes an item In progress to Pending; one that did not go (or may not
// have) leaves the item with the owner and sends one notice, with nothing from the
// mail in it. It returns sent, failed or unknown.
func (a *app) finishSend(ctx context.Context, p repo.Proposal, fail *sendFailure) string {
	state := "sent"
	reason := ""
	if fail != nil {
		state, reason = "failed", oneLine(fail.Text, 400)
		if fail.Doubt {
			state = "unknown"
		}
	}
	var it repo.Item
	var wrote bool
	write := func() error {
		return a.store.Update(ctx, func(tx *repo.Tx) error {
			now := a.now().Unix()
			var ok bool
			it, ok = tx.ItemByID(ctx, p.ItemID)
			if !ok {
				return errNoSuchItem()
			}
			var done bool
			var err error
			if p.Action == replyChat {
				done, err = tx.EndChatSend(ctx, p.ID, state, reason, p.SendRef, now)
			} else {
				done, err = tx.EndSend(ctx, p.ID, state, reason, now)
			}
			if err != nil || !done {
				return err
			}
			wrote = true
			if state == "sent" {
				if err := tx.InsertEvent(ctx, now, it.ID, "system", "reply_sent", map[string]any{"proposal": p.ID, "action": p.Action, "ref": p.SendRef}); err != nil {
					return err
				}
				if it.Stage == stageInProgress {
					if err := tx.SetItemStage(ctx, it.ID, stagePending, now); err != nil {
						return err
					}
					if err := tx.InsertEvent(ctx, now, it.ID, "owner", "pending_set", map[string]any{"item": repo.Number(it.Number), "from": it.Stage, "to": stagePending, "proposal": p.ID}); err != nil {
						return err
					}
				}
				_, err := tx.WithdrawPending(ctx, it.ID, "you replied", now)
				return err
			}
			if err := tx.InsertEvent(ctx, now, it.ID, "system", "reply_failed", map[string]any{"proposal": p.ID, "action": p.Action, "reason": reason, "may_have_gone": state == "unknown"}); err != nil {
				return err
			}
			if it.Stage != stageInProgress && it.Stage != stageNew {
				return tx.SetItemStage(ctx, it.ID, stageInProgress, now)
			}
			return nil
		})
	}
	err := write()
	if err != nil {
		err = write() // the mail has gone or not; the record of it must not be lost to one busy moment
	}
	if err != nil {
		appkit.Error("recording how a send ended", err)
		return state
	}
	if wrote {
		appkit.Logger().Info("reply send ended", "proposal", p.ID, "action", p.Action, "state", state)
		if state != "sent" {
			a.noticeSendFailed(it)
		}
	}
	return state
}

// noticeSendFailed tells the owner's phone that a reply did not go. The notice
// names the item by its number and holds nothing of the mail.
func (a *app) noticeSendFailed(it repo.Item) {
	link := ""
	if base := os.Getenv("BASE_URL"); base != "" {
		link = strings.TrimRight(base, "/") + "/items/" + fmt.Sprint(it.ID)
	}
	if err := a.tell("Could not send", repo.Number(it.Number)+": the reply was not sent, or may not have been. Open ONE to see why.", link); err != nil {
		appkit.Error("sending the notice that a reply could not go", err)
	}
}

// recoverSends is what the app does when it starts: a send whose intent was
// written and whose result was not was cut off in the middle. It is never
// repeated; the thread says it may have gone and the owner looks in the Sent folder.
func (a *app) recoverSends(ctx context.Context) {
	ps, err := a.store.SendsInFlight(ctx)
	if err != nil {
		appkit.Error("looking for sends that were cut off", err)
		return
	}
	for _, p := range ps {
		a.finishSend(ctx, p, &sendFailure{Doubt: true, Text: "ONE stopped while the reply was being sent. It may have gone: check the Sent folder before you try again."})
	}
}

// --- the sent mail comes back through the import ---

// normalText makes two texts comparable: lower case, white space as one space.
func normalText(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// matchSentReply is called inside the import's transaction for a message of
// The owner's own that was not stored before. When it is the reply ONE sent (by the
// Message-ID the app gave a Gmail mail; else by the thread, the time and the
// text, which is all Graph's reply call leaves to go by), the proposal points
// at it and the thread shows it once. It changes nothing else.
func matchSentReply(ctx context.Context, tx *repo.Tx, itemID int64, m repo.Message, messageID int64, now int64) error {
	ps, err := tx.UnmatchedSends(ctx, itemID)
	if err != nil {
		return err
	}
	for _, p := range ps {
		if p.Action == replyChat {
			if parseRecipients(p.Recipient).Network != m.Network || m.ReceivedAt < p.DecidedAt-600 {
				continue
			}
		} else if m.Source != replySource(p.Action) || m.ReceivedAt < p.DecidedAt-600 {
			continue
		}
		byID := p.SendRef != "" && strings.EqualFold(strings.TrimSpace(m.HeaderID), p.SendRef)
		sent, got := normalText(p.FinalText), normalText(m.Text)
		byText := sent != "" && (got == sent || strings.HasPrefix(got, sent))
		if !byID && !byText {
			continue
		}
		wasDoubt := p.SendState == "unknown"
		if err := tx.MatchSent(ctx, p.ID, messageID); err != nil {
			return err
		}
		if wasDoubt {
			if err := tx.InsertEvent(ctx, now, itemID, "system", "reply_sent", map[string]any{"proposal": p.ID, "action": p.Action, "ref": p.SendRef, "confirmed": true}); err != nil {
				return err
			}
		}
		return tx.InsertEvent(ctx, now, itemID, "system", "reply_matched", map[string]any{"proposal": p.ID, "message_id": messageID})
	}
	return nil
}

func replySource(action string) string {
	if action == replyOutlook {
		return srcGraph
	}
	return srcGmail
}

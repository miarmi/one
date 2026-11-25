package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"lab/apps/one/repo"
)

// add_file (SPEC 29, 7): an agent adds a file to an item. It lands in the
// item's one list of files, marked with the agent's name, writes an entry in
// the thread and can be read back with get_file. It is never attached to a
// reply and never sent anywhere. Three ways in: the words of a text file in
// the call; a file that already sits on another item (stored once, by hash);
// or, for a larger or binary file, a one-time upload address.

const (
	maxAgentText      = 200 << 10 // the words of a text file given in the call
	uploadTicketLife  = 5 * time.Minute
	maxUploadTickets  = 200
	uploadPathPrefix  = "/api/uploads/"
	filesAddedEvent   = "file_added"
	maxFilesFromAgent = 100 // a runaway stop: files an agent added to one item
)

// textTypes are the files whose words may come in the call itself.
var textTypes = map[string]string{
	".txt": "text/plain", ".md": "text/markdown", ".csv": "text/csv",
	".ics": "text/calendar", ".html": "text/html", ".json": "application/json",
}

func typeOfName(name string) string {
	if t, ok := textTypes[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}

type uploadGrant struct {
	item    int64
	agent   string
	name    string
	run     string
	expires time.Time
}

// uploadGrants are the addresses handed out and not yet used. They live in
// memory: a restart voids them, which is what a few minutes' life allows.
type uploadGrants struct {
	mu sync.Mutex
	m  map[string]uploadGrant
}

func (g *uploadGrants) issue(now time.Time, gr uploadGrant) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]uploadGrant{}
	}
	for k, v := range g.m {
		if now.After(v.expires) {
			delete(g.m, k)
		}
	}
	if len(g.m) >= maxUploadTickets {
		return "", conflict("Too many upload addresses are waiting to be used. Use one, or wait a few minutes.")
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	key := hex.EncodeToString(b)
	gr.expires = now.Add(uploadTicketLife)
	g.m[key] = gr
	return key, nil
}

// take uses an address up: it works once, whatever comes of the upload.
func (g *uploadGrants) take(now time.Time, key string) (uploadGrant, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	gr, ok := g.m[key]
	delete(g.m, key)
	return gr, ok && !now.After(gr.expires)
}

type fileAdd struct {
	Name     string
	Text     string
	HasText  bool
	FromItem string
	FileID   int64
}

type fileAdded struct {
	Item   repo.Item
	FileID int64
	Name   string
	Size   int64
	Same   bool
	Upload string // the address, when the file comes by upload
}

// stored is a file already in the pool, ready to be listed on an item.
type stored struct {
	name, ctype, sha, rel string
	size                  int64
	created               bool
}

// addFile does the first two ways in, and hands out the address for the third.
func (a *app) addFile(ctx context.Context, x actor, ref string, in fileAdd) (fileAdded, error) {
	var res fileAdded
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	name := cleanFileName(in.Name)
	if in.HasText && in.FromItem != "" {
		return res, bad("Give either text or from_item, not both.")
	}
	var st stored
	switch {
	case in.HasText:
		if name == "" {
			return res, bad("name is required: the file's name with its ending, like notes.md.")
		}
		if _, ok := textTypes[strings.ToLower(path.Ext(name))]; !ok {
			return res, bad("Text can be given in the call only for a txt, md, csv, ics, html or json file. For anything else ask for an upload address: give no text and no from_item.")
		}
		text := strings.ToValidUTF8(in.Text, "")
		if strings.TrimSpace(text) == "" {
			return res, bad("The text is empty.")
		}
		if len(text) > maxAgentText {
			return res, bad(fmt.Sprintf("Text in the call can be at most %d kB. For a larger file ask for an upload address: give no text and no from_item.", maxAgentText>>10))
		}
		sha, rel, size, created, err := a.imp.pool.write(strings.NewReader(text))
		if err != nil {
			return res, err
		}
		st = stored{name: name, ctype: typeOfName(name), sha: sha, rel: rel, size: size, created: created}
	case in.FromItem != "":
		if in.FileID <= 0 {
			return res, bad("file_id is required with from_item: get_item on that item lists its files.")
		}
	default:
		if name == "" {
			return res, bad("name is required for an upload: the file's name with its ending.")
		}
		// The third way: the item is checked now and again when the file arrives.
		it, err := a.findItemW(ctx, a.store.Q, ref)
		if err != nil {
			return res, a.logRefusal(ctx, x, 0, err)
		}
		key, err := a.uploads.issue(a.now(), uploadGrant{item: it.ID, agent: x.name, name: name, run: repo.RunOf(ctx)})
		if err != nil {
			return res, err
		}
		res.Item, res.Name, res.Upload = it, name, uploadPathPrefix+key
		return res, nil
	}
	return a.listAgentFile(ctx, x, ref, in, st)
}

// listAgentFile puts a stored file (or one that sits on another item) on the
// item and writes the entry.
func (a *app) listAgentFile(ctx context.Context, x actor, ref string, in fileAdd, st stored) (fileAdded, error) {
	var res fileAdded
	now := a.now().Unix()
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		it, err := a.findItemW(ctx, tx.Q, ref)
		if err != nil {
			return err
		}
		res.Item = it
		var from repo.Item
		if in.FromItem != "" {
			if from, err = a.findItem(ctx, tx.Q, in.FromItem); err != nil {
				return err
			}
			if from.TestRun != it.TestRun {
				return runRefused() // a file never crosses between a test run and the real records
			}
			files, err := tx.FileList(ctx, from.ID)
			if err != nil {
				return err
			}
			found := false
			for _, f := range files {
				if f.ID != in.FileID {
					continue
				}
				found = true
				if f.Status != "saved" || f.Path == "" {
					return conflict("That file has not been fetched yet, so it cannot be added.")
				}
				name := cleanFileName(in.Name)
				if name == "" {
					name = f.Filename
				}
				st = stored{name: name, ctype: f.ContentType, sha: f.SHA, rel: f.Path, size: f.Size}
			}
			if !found {
				return &userErr{http.StatusNotFound, "that item has no file with that file_id"}
			}
		}
		res.Name, res.Size = st.name, st.size
		files, err := tx.FileList(ctx, it.ID)
		if err != nil {
			return err
		}
		mine := 0
		for _, f := range files {
			if f.SHA == st.sha && f.Filename == st.name && f.MessageID == 0 && f.Source == x.id() {
				res.FileID, res.Same = f.ID, true
				return nil // the same file under the same name from the same agent: already there
			}
			if f.MessageID == 0 && f.Source != "owner" {
				mine++
			}
		}
		if mine >= maxFilesFromAgent {
			return conflict(fmt.Sprintf("This item already holds %d files added by agents.", maxFilesFromAgent))
		}
		id, err := tx.InsertOwnFile(ctx, repo.File{ItemID: it.ID, Source: x.id(), Part: "agent-" + newGroupID(), Filename: st.name,
			ContentType: st.ctype, Size: st.size, SHA: st.sha, Path: st.rel, AddedAt: now})
		if err != nil {
			return err
		}
		res.FileID = id
		if err := tx.PoolUsed(ctx, st.rel, st.created, it.ID); err != nil {
			return err
		}
		data := map[string]any{"files": []string{st.name}, "file_id": id}
		if in.FromItem != "" {
			data["from_item"] = repo.Number(from.Number)
		}
		return tx.InsertEvent(ctx, now, it.ID, x.id(), filesAddedEvent, data)
	})
	return res, a.logRefusal(ctx, x, res.Item.ID, err)
}

func fileAddedJSON(res fileAdded) map[string]any {
	out := map[string]any{"ok": true, "id": repo.Number(res.Item.Number), "name": res.Name}
	if res.Upload != "" {
		url := res.Upload
		if base := strings.TrimRight(os.Getenv("BASE_URL"), "/"); base != "" {
			url = base + url
		}
		out["upload"] = map[string]any{"method": "PUT", "url": url, "valid_for_seconds": int(uploadTicketLife.Seconds()), "uses": 1, "max_bytes": maxWriteFile,
			"how": "Send the bytes of the file as the body of one PUT request to this address, with the same Authorization: Bearer header as for the other tools. The file is listed on the item as " + res.Name + "."}
		return out
	}
	out["file_id"], out["size"] = res.FileID, res.Size
	if res.Same {
		out["unchanged"] = true
	}
	return out
}

type addFileBody struct {
	Agent    string  `json:"agent"`
	Name     string  `json:"name"`
	Text     *string `json:"text"`
	FromItem string  `json:"from_item"`
	FileID   int64   `json:"file_id"`
}

func (a *app) apiAddFile(w http.ResponseWriter, r *http.Request) {
	var b addFileBody
	if !readJSONMax(w, r, &b, maxFileCallBody) {
		return
	}
	in := fileAdd{Name: b.Name, FromItem: b.FromItem, FileID: b.FileID}
	if b.Text != nil {
		in.Text, in.HasText = *b.Text, true
	}
	res, err := a.addFile(r.Context(), agentActor(b.Agent), r.PathValue("id"), in)
	if err != nil {
		answerErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fileAddedJSON(res))
}

// apiUpload takes the bytes of a file to an address handed out by add_file.
// The agents' token has been checked before this runs; the address is bound to
// the item, the agent and the test run it was made for, works once and
// expires in minutes. The bytes are streamed to the pool.
func (a *app) apiUpload(w http.ResponseWriter, r *http.Request) {
	gr, ok := a.uploads.take(a.now(), r.PathValue("key"))
	if !ok {
		apiError(w, http.StatusNotFound, "This upload address is not valid: it was used already, it has expired or it never existed. Ask add_file for a new one.")
		return
	}
	if h, err := runFrom(r.Header, ""); err != nil || h != gr.run {
		answerErr(w, runRefused())
		return
	}
	ctx := repo.WithRun(r.Context(), gr.run)
	x := agentActor(gr.agent)
	it, found := a.store.ItemByID(ctx, gr.item)
	if !found {
		answerErr(w, errNoSuchItem())
		return
	}
	pw, err := a.imp.pool.newWriter()
	if err != nil {
		answerErr(w, err)
		return
	}
	n, err := io.Copy(pw, io.LimitReader(r.Body, maxWriteFile+1))
	if err != nil {
		pw.abort()
		apiError(w, http.StatusBadRequest, "The file could not be read to its end.")
		return
	}
	if n > maxWriteFile {
		pw.abort()
		apiError(w, http.StatusRequestEntityTooLarge, gr.name+" is larger than 20 MB.")
		return
	}
	if n == 0 {
		pw.abort()
		apiError(w, http.StatusBadRequest, gr.name+" is empty.")
		return
	}
	sha, rel, size, created, err := pw.finish()
	if err != nil {
		answerErr(w, err)
		return
	}
	res, err := a.listAgentFile(ctx, x, repo.Number(it.Number), fileAdd{}, stored{name: gr.name, ctype: typeOfName(gr.name), sha: sha, rel: rel, size: size, created: created})
	if err != nil {
		answerErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fileAddedJSON(res))
}

var attachTools = []mcpTool{
	{"add_file", "Add a file to an item",
		"Add a file to an item (id), under your name. It lands in the item's one list of files, next to the files that came with the messages, with your name as its source; the thread shows an entry \"Attached: <name>\" and get_file reads it back. It is never attached to a reply and never sent anywhere: a reply still goes only through propose_reply and the owner's Approve. Three ways. (a) text with a name (the file's name with its ending): the words of a txt, md, csv, ics, html or json file, at most 200 kB. (b) from_item and file_id: a file that already sits on another item (get_item lists the files with their file_id); it is stored once and listed on this item too; give name only to rename it. (c) For a larger or binary file give name and neither text nor from_item: the answer is a one-time upload address under /api/ for one PUT request with the file's bytes as the body, with the same Authorization: Bearer header; it works once, for this item and this agent, for five minutes, up to 20 MB. The same file under the same name twice returns the first one. Takes effect at once; changes nothing else.",
		schema(map[string]any{"id": itemT, "name": strT, "text": strT, "from_item": itemT, "file_id": intT, "agent": agentT}, "id"), hints("Add a file to an item", false, false, false)},
}

func init() { mcpTools = append(mcpTools, attachTools...) }

func (a *app) callAttachTool(ctx context.Context, name string, args json.RawMessage) (text string, isErr, known bool) {
	if name != "add_file" {
		return "", false, false
	}
	var in struct {
		ID       json.RawMessage `json:"id"`
		Name     string          `json:"name"`
		Text     *string         `json:"text"`
		FromItem json.RawMessage `json:"from_item"`
		FileID   int64           `json:"file_id"`
		Agent    string          `json:"agent"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "The arguments must be a JSON object.", true, true
	}
	ref, ok := itemRef(in.ID)
	if !ok {
		return "id must be an item number like Q-0012.", true, true
	}
	body := map[string]any{"name": in.Name, "agent": in.Agent}
	if in.Text != nil {
		body["text"] = *in.Text
	}
	if len(in.FromItem) > 0 && string(in.FromItem) != "null" {
		from, ok := itemRef(in.FromItem)
		if !ok {
			return "from_item must be an item number like Q-0012.", true, true
		}
		body["from_item"] = from
		body["file_id"] = in.FileID
	}
	b, _ := json.Marshal(body)
	text, isErr = a.callAPI(ctx, "POST", "/api/items/"+ref+"/files", b)
	return text, isErr, true
}

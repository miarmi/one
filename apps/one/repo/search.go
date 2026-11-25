package repo

import (
	"context"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Full-text search (SPEC 26). The driver in this repo is built with SQLite's
// FTS4 and not with FTS5 (that needs a build tag the lab's build script does
// not set), so the index is an FTS4 table with the unicode61 tokenizer, which
// folds case and accents. It holds one row for every message, every note or
// comment in a thread and every saved note; triggers keep it in step with the
// tables, so no code path can forget to index. The index is a copy made for
// searching: the text itself lives in messages, events and notes.

// maxIndexed bounds the text of one message in the index; the message itself
// is kept whole.
const maxIndexed = 200000

const finderSchema = `
CREATE VIRTUAL TABLE IF NOT EXISTS finder USING fts4(title, body, kind, item, ref, at, who,
	notindexed=kind, notindexed=item, notindexed=ref, notindexed=at, notindexed=who, tokenize=unicode61);
CREATE TRIGGER IF NOT EXISTS finder_msg_ai AFTER INSERT ON messages BEGIN
	INSERT INTO finder (rowid, title, body, kind, item, ref, at, who)
	VALUES (new.id * 4, new.subject, substr(new.text, 1, 200000), CASE WHEN new.network = 'mail' THEN 'mail' ELSE 'chat' END, new.item_id, new.id, new.received_at, new.sender);
END;
CREATE TRIGGER IF NOT EXISTS finder_msg_au AFTER UPDATE OF subject, text, network, item_id ON messages BEGIN
	DELETE FROM finder WHERE rowid = old.id * 4;
	INSERT INTO finder (rowid, title, body, kind, item, ref, at, who)
	VALUES (new.id * 4, new.subject, substr(new.text, 1, 200000), CASE WHEN new.network = 'mail' THEN 'mail' ELSE 'chat' END, new.item_id, new.id, new.received_at, new.sender);
END;
CREATE TRIGGER IF NOT EXISTS finder_event_ai AFTER INSERT ON events
WHEN new.item_id IS NOT NULL AND new.type IN ('noted', 'commented') AND json_valid(new.data) AND json_extract(new.data, '$.text') IS NOT NULL BEGIN
	INSERT INTO finder (rowid, title, body, kind, item, ref, at, who)
	VALUES (new.id * 4 + 1, '', json_extract(new.data, '$.text'), 'comment', new.item_id, new.id, new.at, new.actor);
END;
CREATE TRIGGER IF NOT EXISTS finder_note_ai AFTER INSERT ON notes BEGIN
	INSERT INTO finder (rowid, title, body, kind, item, ref, at, who)
	VALUES (new.id * 4 + 2, new.title, new.body, 'note', COALESCE(new.item_id, 0), new.id, new.updated_at, new.updated_by);
END;
CREATE TRIGGER IF NOT EXISTS finder_msg_ad AFTER DELETE ON messages BEGIN
	DELETE FROM finder WHERE rowid = old.id * 4;
END;
CREATE TRIGGER IF NOT EXISTS finder_event_ad AFTER DELETE ON events
WHEN old.item_id IS NOT NULL AND old.type IN ('noted', 'commented') BEGIN
	DELETE FROM finder WHERE rowid = old.id * 4 + 1;
END;
CREATE TRIGGER IF NOT EXISTS finder_item_ad AFTER DELETE ON items BEGIN
	DELETE FROM finder WHERE rowid = old.id * 4 + 3;
END;
CREATE TRIGGER IF NOT EXISTS finder_note_ad AFTER DELETE ON notes BEGIN
	DELETE FROM finder WHERE rowid = old.id * 4 + 2;
END;
CREATE TRIGGER IF NOT EXISTS finder_note_au AFTER UPDATE ON notes BEGIN
	DELETE FROM finder WHERE rowid = old.id * 4 + 2;
	INSERT INTO finder (rowid, title, body, kind, item, ref, at, who)
	VALUES (new.id * 4 + 2, new.title, new.body, 'note', COALESCE(new.item_id, 0), new.id, new.updated_at, new.updated_by);
END;
`

// ensureFinder makes the index and its triggers, and fills it from what is
// already stored when it is new (a database made before search existed).
func ensureFinder(db schemaDB) error {
	var have int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name = 'finder'").Scan(&have); err != nil {
		return err
	}
	if _, err := db.Exec(finderSchema); err != nil {
		return err
	}
	if have > 0 {
		return nil
	}
	_, err := db.Exec(`
INSERT INTO finder (rowid, title, body, kind, item, ref, at, who)
	SELECT id * 4, subject, substr(text, 1, 200000), CASE WHEN network = 'mail' THEN 'mail' ELSE 'chat' END, item_id, id, received_at, sender FROM messages;
INSERT INTO finder (rowid, title, body, kind, item, ref, at, who)
	SELECT id * 4 + 1, '', json_extract(data, '$.text'), 'comment', item_id, id, at, actor FROM events
	WHERE item_id IS NOT NULL AND type IN ('noted', 'commented') AND json_valid(data) AND json_extract(data, '$.text') IS NOT NULL;
INSERT INTO finder (rowid, title, body, kind, item, ref, at, who)
	SELECT id * 4 + 2, title, body, 'note', COALESCE(item_id, 0), id, updated_at, updated_by FROM notes;`)
	return err
}

// MatchExpr turns what a person typed into an FTS4 query: every word must
// be there, the last one may be only the start of a word. Operators in the
// text are plain words. It returns "" when there is no word to look for.
func MatchExpr(text string) string {
	words := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(words) > 8 {
		words = words[:8]
	}
	var parts []string
	for i, w := range words {
		if r := []rune(w); len(r) > 40 {
			w = string(r[:40])
		}
		if i == len(words)-1 {
			// FTS4 takes a prefix only on a bare word; lower case, so it can
			// never be one of the operators.
			parts = append(parts, strings.ToLower(w)+"*")
			continue
		}
		parts = append(parts, `"`+w+`"`)
	}
	return strings.Join(parts, " ")
}

// Hit is one result of a search.
type Hit struct {
	Kind    string // mail, chat, comment or note
	ItemID  int64  // 0 for a note that belongs to no item
	Ref     int64  // the id of the message, the event or the note
	At      int64
	Who     string
	Title   string
	Snippet string // the matching words are between \x01 and \x02
	Number  int64  // the item's number (0 when none)
	Subject string // the item's subject
	Stage   string
}

// SearchQuery is what to look for.
type SearchQuery struct {
	Match         string  // from MatchExpr
	Kind          string  // when set, only this kind (mail, chat, event, comment or note)
	PersonIDs     []int64 // when set, only what concerns these people
	IncludeHidden bool    // items that wait for Claude's first look (agents see them, the owner's page does not)
	Limit, Offset int
}

// Search runs a query, newest first. It reads at most Limit rows.
func (q Q) Search(ctx context.Context, sq SearchQuery) ([]Hit, error) {
	if sq.Match == "" {
		return nil, nil
	}
	args := []any{sq.Match}
	where := "finder MATCH ?"
	if !sq.IncludeHidden {
		where += " AND (i.id IS NULL OR i.hidden = 0)"
	}
	if sq.Kind != "" {
		where += " AND finder.kind = ?"
		args = append(args, sq.Kind)
	}
	if len(sq.PersonIDs) > 0 {
		in := strings.TrimSuffix(strings.Repeat("?,", len(sq.PersonIDs)), ",")
		handles := "SELECT lower(value) FROM person_handles WHERE person_id IN (" + in + ")"
		where += ` AND ((finder.kind IN ('mail', 'chat') AND lower(finder.who) IN (` + handles + `))
			OR (finder.kind = 'note' AND finder.ref IN (SELECT id FROM notes WHERE person_id IN (` + in + `)))
			OR (finder.kind = 'comment' AND finder.item IN (SELECT item_id FROM messages WHERE lower(sender) IN (` + handles + `))))`
		for i := 0; i < 3; i++ {
			for _, id := range sq.PersonIDs {
				args = append(args, id)
			}
		}
	}
	args = append(args, sq.Limit, sq.Offset)
	rows, err := q.x.QueryContext(ctx, `SELECT finder.kind, finder.item, finder.ref, finder.at, finder.who, finder.title, snippet(finder, char(1), char(2), '…', 1, 14),
		COALESCE(i.number, 0), COALESCE(i.subject, ''), COALESCE(i.stage, '')
		FROM finder LEFT JOIN items i ON i.id = finder.item WHERE `+where+` ORDER BY finder.at DESC, finder.rowid DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Kind, &h.ItemID, &h.Ref, &h.At, &h.Who, &h.Title, &h.Snippet, &h.Number, &h.Subject, &h.Stage); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Fold lower-cases a text and takes the accents off its letters, one rune for
// one rune, so a position in the folded text is the same position in the
// original ("Nájemníčka" and "najemnicka" fold alike). The index does the same
// for words (SQLite's unicode61 tokenizer removes diacritics both in what is
// stored and in what is asked), so what the index finds, Fold can mark.
func Fold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		b.WriteRune(foldRune(r))
	}
	return b.String()
}

func foldRune(r rune) rune {
	if r < 0x80 {
		return unicode.ToLower(r)
	}
	for _, d := range norm.NFD.String(string(r)) {
		if !unicode.Is(unicode.Mn, d) {
			return unicode.ToLower(d)
		}
	}
	return unicode.ToLower(r)
}

// SearchPeople finds the people whose name, what the owner calls them, relation
// or handle contains the text, without regard to case or accents (SQLite's
// LIKE knows neither for non-ASCII letters), at most limit of them.
func (q Q) SearchPeople(ctx context.Context, text string, limit int) ([]Person, error) {
	want := Fold(strings.TrimSpace(text))
	if want == "" {
		return nil, nil
	}
	all, err := q.listPeople(ctx, "", 100000, 0, true)
	if err != nil {
		return nil, err
	}
	var out []Person
	for _, p := range all {
		hay := p.Name + " " + p.CallName + " " + p.Relation
		for _, h := range p.Handles {
			hay += " " + h.Value
		}
		if strings.Contains(Fold(hay), want) {
			out = append(out, p)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

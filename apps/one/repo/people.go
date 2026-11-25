package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// People (SPEC 25). A person has a name (from mail, or typed by the owner), what
// The owner calls them, who they are to him, a protected mark and any number of
// handles: a mail address, a number or a name on a network.

// Handle is one way a person is reached on one network.
type Handle struct{ Network, Value string }

// Person is one entry of the People page.
type Person struct {
	ID         int64
	Name       string
	CallName   string
	Relation   string
	Protected  bool
	HasPicture bool
	UpdatedBy  string
	UpdatedAt  int64
	Handles    []Handle
}

// Shown is the name the owner reads: what he calls the person, else their name.
func (p Person) Shown() string {
	if p.CallName != "" {
		return p.CallName
	}
	return p.Name
}

const personCols = `p.id, p.name, p.call_name, p.relation, p.protected, EXISTS (SELECT 1 FROM person_pictures x WHERE x.person_id = p.id), p.updated_by, p.updated_at`

func scanPerson(row interface{ Scan(...any) error }) (Person, error) {
	var p Person
	var prot, pic int
	err := row.Scan(&p.ID, &p.Name, &p.CallName, &p.Relation, &prot, &pic, &p.UpdatedBy, &p.UpdatedAt)
	p.Protected, p.HasPicture = prot == 1, pic == 1
	return p, err
}

// NormalHandle is the stored form of a handle: addresses are lower case.
func NormalHandle(network, value string) Handle {
	network, value = strings.ToLower(strings.TrimSpace(network)), strings.TrimSpace(value)
	if network == "mail" {
		value = strings.ToLower(value)
	}
	return Handle{network, value}
}

// loadHandles fills the handles of the given people.
func (q Q) loadHandles(ctx context.Context, people []Person) error {
	if len(people) == 0 {
		return nil
	}
	idx := map[int64]int{}
	args := make([]any, len(people))
	for i, p := range people {
		idx[p.ID] = i
		args[i] = p.ID
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(people)), ",")
	rows, err := q.x.QueryContext(ctx, `SELECT person_id, network, value FROM person_handles WHERE person_id IN (`+in+`) ORDER BY network <> 'mail', network, value`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var h Handle
		if err := rows.Scan(&id, &h.Network, &h.Value); err != nil {
			return err
		}
		people[idx[id]].Handles = append(people[idx[id]].Handles, h)
	}
	return rows.Err()
}

func likeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

func peopleWhere(filter string) (string, []any) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return "1 = 1", nil
	}
	pat := "%" + likeEscape(filter) + "%"
	return `(p.name LIKE ? ESCAPE '\' OR p.call_name LIKE ? ESCAPE '\' OR p.relation LIKE ? ESCAPE '\'
		OR EXISTS (SELECT 1 FROM person_handles h WHERE h.person_id = p.id AND h.value LIKE ? ESCAPE '\'))`, []any{pat, pat, pat, pat}
}

// ListPeople lists people by the name the owner reads, those he marked protected
// first. filter looks in names, what he calls them, relation and handles.
func (q Q) ListPeople(ctx context.Context, filter string, limit, offset int) ([]Person, error) {
	return q.listPeople(ctx, filter, limit, offset, true)
}

func (q Q) listPeople(ctx context.Context, filter string, limit, offset int, withHandles bool) ([]Person, error) {
	where, args := peopleWhere(filter)
	args = append(args, limit, offset)
	rows, err := q.x.QueryContext(ctx, `SELECT `+personCols+` FROM people p WHERE `+where+`
		ORDER BY p.protected DESC, lower(COALESCE(NULLIF(p.call_name, ''), p.name)), p.id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	var out []Person
	for rows.Next() {
		p, err := scanPerson(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !withHandles {
		return out, nil
	}
	return out, q.loadHandles(ctx, out)
}

// CountPeople counts the people that match the filter.
func (q Q) CountPeople(ctx context.Context, filter string) int {
	where, args := peopleWhere(filter)
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM people p WHERE `+where, args...).Scan(&n)
	return n
}

func (q Q) PersonByID(ctx context.Context, id int64) (Person, bool) {
	p, err := scanPerson(q.x.QueryRowContext(ctx, `SELECT `+personCols+` FROM people p WHERE p.id = ?`, id))
	if err != nil {
		return Person{}, false
	}
	list := []Person{p}
	if q.loadHandles(ctx, list) != nil {
		return Person{}, false
	}
	return list[0], true
}

// FindPeople finds the people whose name, what the owner calls them or handle
// contains the text, at most limit of them.
func (q Q) FindPeople(ctx context.Context, text string, limit int) ([]Person, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	return q.ListPeople(ctx, text, limit, 0)
}

// PersonRef is what a row or a message needs of a person.
type PersonRef struct {
	ID         int64
	Shown      string
	HasPicture bool
}

// PeopleByHandles finds the people for the given handles (network, lower-case value).
func (q Q) PeopleByHandles(ctx context.Context, handles []Handle) (map[Handle]PersonRef, error) {
	out := map[Handle]PersonRef{}
	for i := 0; i < len(handles); i += 200 {
		part := handles[i:min(i+200, len(handles))]
		args := make([]any, 0, len(part)*2)
		cond := make([]string, len(part))
		for j, h := range part {
			cond[j] = "(h.network = ? AND lower(h.value) = ?)"
			args = append(args, h.Network, strings.ToLower(h.Value))
		}
		rows, err := q.x.QueryContext(ctx, `SELECT h.network, lower(h.value), p.id, COALESCE(NULLIF(p.call_name, ''), p.name), EXISTS (SELECT 1 FROM person_pictures x WHERE x.person_id = p.id)
			FROM person_handles h JOIN people p ON p.id = h.person_id WHERE `+strings.Join(cond, " OR "), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h Handle
			var r PersonRef
			var pic int
			if err := rows.Scan(&h.Network, &h.Value, &r.ID, &r.Shown, &pic); err != nil {
				rows.Close()
				return nil, err
			}
			r.HasPicture = pic == 1
			out[h] = r
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ErrHandleTaken says a handle already belongs to another person.
type ErrHandleTaken struct {
	Handle Handle
	Owner  string
}

func (e *ErrHandleTaken) Error() string { return e.Handle.Value + " already belongs to " + e.Owner }

// AddPerson makes a person typed by the owner.
func (q Q) AddPerson(ctx context.Context, p Person, by string, at int64) (int64, error) {
	res, err := q.x.ExecContext(ctx, `INSERT INTO people (name, call_name, relation, protected, updated_by, updated_at, test_run) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.CallName, p.Relation, b2i(p.Protected), by, at, RunOf(ctx))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdatePerson changes what the owner can change about a person.
func (q Q) UpdatePerson(ctx context.Context, p Person, by string, at int64) error {
	if err := q.CheckRow(ctx, "people", p.ID); err != nil {
		return err
	}
	res, err := q.x.ExecContext(ctx, `UPDATE people SET name=?, call_name=?, relation=?, protected=?, updated_by=?, updated_at=? WHERE id=?`,
		p.Name, p.CallName, p.Relation, b2i(p.Protected), by, at, p.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no such person")
	}
	return nil
}

// SetHandles makes the person's handles exactly this list. A handle that
// belongs to someone else is not taken over: it is refused with the owner's name.
func (q Q) SetHandles(ctx context.Context, personID int64, handles []Handle) error {
	mark, err := q.runForRow(ctx, "people", personID)
	if err != nil {
		return err
	}
	want := map[Handle]bool{}
	for _, h := range handles {
		h = NormalHandle(h.Network, h.Value)
		want[h] = true
		var owner int64
		var name string
		err := q.x.QueryRowContext(ctx, `SELECT h.person_id, COALESCE(NULLIF(p.call_name, ''), p.name) FROM person_handles h JOIN people p ON p.id = h.person_id
			WHERE h.network = ? AND lower(h.value) = lower(?)`, h.Network, h.Value).Scan(&owner, &name)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := q.x.ExecContext(ctx, `INSERT INTO person_handles (person_id, network, value, test_run) VALUES (?, ?, ?, ?)`, personID, h.Network, h.Value, mark); err != nil {
				return err
			}
		case err != nil:
			return err
		case owner != personID:
			return &ErrHandleTaken{Handle: h, Owner: name}
		}
	}
	rows, err := q.x.QueryContext(ctx, `SELECT network, value FROM person_handles WHERE person_id = ?`, personID)
	if err != nil {
		return err
	}
	var drop []Handle
	for rows.Next() {
		var h Handle
		if err := rows.Scan(&h.Network, &h.Value); err != nil {
			rows.Close()
			return err
		}
		if !want[NormalHandle(h.Network, h.Value)] {
			drop = append(drop, h)
		}
	}
	rows.Close()
	for _, h := range drop {
		if _, err := q.x.ExecContext(ctx, `DELETE FROM person_handles WHERE person_id = ? AND network = ? AND value = ?`, personID, h.Network, h.Value); err != nil {
			return err
		}
	}
	return nil
}

// SetPicture keeps the owner's own picture for a person (replacing the old one).
func (q Q) SetPicture(ctx context.Context, personID int64, contentType string, data []byte, at int64) error {
	mark, err := q.runForRow(ctx, "people", personID)
	if err != nil {
		return err
	}
	_, err = q.x.ExecContext(ctx, `INSERT INTO person_pictures (person_id, content_type, data, updated_at, test_run) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(person_id) DO UPDATE SET content_type = excluded.content_type, data = excluded.data, updated_at = excluded.updated_at`, personID, contentType, data, at, mark)
	return err
}

func (q Q) RemovePicture(ctx context.Context, personID int64) error {
	_, err := q.x.ExecContext(ctx, `DELETE FROM person_pictures WHERE person_id = ?`, personID)
	return err
}

func (q Q) Picture(ctx context.Context, personID int64) (contentType string, data []byte, at int64, ok bool) {
	err := q.x.QueryRowContext(ctx, `SELECT content_type, data, updated_at FROM person_pictures WHERE person_id = ?`, personID).Scan(&contentType, &data, &at)
	return contentType, data, at, err == nil
}

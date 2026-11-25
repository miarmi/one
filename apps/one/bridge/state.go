package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// state is the bridge's whole memory: which version of each message ONE has
// already been given (a short hash, never the text), and what became of each
// reply. It is written before a reply leaves and after ONE has answered a
// batch, so a restart sends nothing twice.
type state struct {
	mu      sync.Mutex
	path    string
	Chats   map[string]*chatState  `json:"chats"`
	Replies map[string]*replyState `json:"replies"` // by id; what the owner wrote himself is under "p<id>"
	// Files are the attachments ONE asked for that could not be fetched yet:
	// they are tried again, never dropped.
	Files map[string]*fileWait `json:"files"`
	// FailedGroups are the writes of the owner's of which a message did not go: the
	// messages after it are not sent.
	FailedGroups map[string]bool `json:"failed_groups"`
}

type chatState struct {
	Activity string `json:"activity"`
	// Cursor is the newest position in the chat that ONE has taken everything up
	// to: the next read asks only for what comes after it. It moves only after
	// ONE has answered a batch, so a restart loses and repeats nothing.
	Cursor string            `json:"cursor,omitempty"`
	Msgs   map[string]string `json:"msgs"`
}

type replyState struct {
	State    string `json:"state"` // sending, watching, sent, failed, unknown
	Error    string `json:"error,omitempty"`
	Message  string `json:"message_id,omitempty"`
	Reported bool   `json:"reported"`
	// While Beeper has accepted the message and not yet said it went: the chat
	// it really went to, and when it was accepted (unix seconds).
	Chat  string `json:"chat,omitempty"`
	Since int64  `json:"since,omitempty"`
}

// fileWait is an attachment still to be brought to ONE.
type fileWait struct {
	Network string `json:"network"`
	Chat    string `json:"chat"`
	Message string `json:"message"`
	Att     string `json:"att"`
	Src     string `json:"src"`
	Reason  string `json:"reason"`
}

func loadState(path string) (*state, error) {
	s := &state{path: path, Chats: map[string]*chatState{}, Replies: map[string]*replyState{}, Files: map[string]*fileWait{}, FailedGroups: map[string]bool{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Chats == nil {
		s.Chats = map[string]*chatState{}
	}
	if s.Replies == nil {
		s.Replies = map[string]*replyState{}
	}
	if s.Files == nil {
		s.Files = map[string]*fileWait{}
	}
	if s.FailedGroups == nil {
		s.FailedGroups = map[string]bool{}
	}
	return s, nil
}

// save writes the state atomically with owner-only rights.
func (s *state) save() error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

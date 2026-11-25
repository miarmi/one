# ONE

The owner's one page for everything outside work that needs him, laid out like the
second version of the tool he built for his work: one list of rows in five
stages, each row with who it is from, what it is about, and a thread where he,
Claude and other agents talk it through. The app fetches his mail, chats and
calendars by itself; Claude only reviews what came in. Nothing is sent or
changed for him without his Approve.

Designed with the owner, modelled on the second
version of the tool he built for his work. ONE starts empty and has no
priority: the five stages replace any pills or flags.

## What it must do

1. **One list in five stages.** Opening the app shows a page titled "ONE"
   with the items in groups by stage: New, In progress, Pending and Resolved.
   Closed is not in the list; it has a page of its own (point 27). All groups
   are folded when the page opens. A folded group is one header line with
   its count, and opens on a click. Agreed with the owner. The
   header also says `<n> to approve` when Claude has proposals in that group,
   and on Pending the longest wait. A group opened shows at least ten rows on
   a laptop screen. The heading says, as two separate numbers, how many items
   are new and how many are in progress (the owner: split them). The
   page says
   which environment it runs in. There is no priority and no filter. The top
   of the page respects vertical space (the owner): the app's name, the connections dot, the links, the search,
   the environment and the heading with its numbers take one line on a
   laptop and at most two on a phone, so the list starts right under them. The name `ONE` at the top of every
   page is a link to the list (the owner).
   **On a laptop the page has three panes and a ribbon** (the owner, choosing the third of three drawn directions). This replaces
   the folded groups and the one-line top on a laptop; a phone keeps the
   folded groups and the top of at most two lines described above.
   - Left pane: the stages New, In progress, Pending, Resolved and Closed,
     one under another, each with its count, and `<n> to approve` on a
     stage where Claude has proposals. One stage is chosen; the page opens
     on New. Closed in this pane leads to the Closed page (point 27).
   - At the bottom of the left pane, drawn from the bottom up, the
     connections : one line for each
     source and one for Claude, and not just OK : each says when it was last read
     and how much came today. A source that has stopped or needs login, and
     items waiting for Claude, stand out in colour. A source that is late or
     has an error says so too, with when it was last read, and stands out in
     colour like a stopped one (the owner). What the strips of
     point 11, 24 and 31 say on a phone is said here on a laptop: the
     stopped source, the waiting items, the paused review and the night
     check's line with its `Dismiss`. Each line leads to Connectors,
     except the lines of Gmail, Office mail and the chat bridge, which
     show only that connection's rows (point 23, the owner); the
     status dot in the top bar and the calendars' lines still lead to
     Connectors.
   - On a phone, when the notices in the top strip do not fit on one line,
     the least important are shortened and then hidden behind "+n more"; a
     tap on it opens Connectors (the owner).
     - Exact wording: `+<n> more`.
   - Middle pane: the search with its suggestions (point 28) at the top,
     under the ribbon , then the
     chosen stage's name with how many items it holds, then its rows,
     newest first. A row opens in place as in point 2. Rows carry no
     buttons of their own except `Approve` and `Dismiss` on a proposal,
     which stay in the row.
   - Right pane: the agenda, always there on a laptop (no `Agenda` button),
     and at its bottom, drawn from the bottom up, the to-dos with the box
     to add one .
   - The ribbon at the top :
     the name ONE, then with an icon and a word each: what can be done with
     the chosen row (open it, reply, mark pending, resolve, make a to-do),
     then the pages, then refresh. The row's actions are those the row
     offers today for its stage; one that does not apply is greyed.
   - One type everywhere : the same family in all panes and the ribbon, one size
     for text and one smaller size for times and side notes, two weights.
   - Exact wording on a laptop: stage names as above plus `Closed`; the
     middle pane's heading `<stage>` with `<n> items, newest first` (or
     `1 item`, or `Nothing in <stage>.`); connections heading `Connections`,
     a line `<source>` with `read <n> min ago` (or `read at <time>`) and
     `<n> new today` (calendars: `<n> changes`), a stopped source
     `stopped` with `last heard <time>`, one needing login `needs login`,
     a late source `late` and one with an error `error`, each with
     `read <time>`,
     Claude's line `Claude` with `<n> waiting` and `last read <time>`;
     ribbon words `Open`, `Reply`, `Pending`, `Resolve`, `Reopen`, `To do`,
     `Notes`, `People`, `Connectors`, `Settings`, `Refresh`; agenda
     headings `Agenda` and `To do`.
   - Exact wording: browser tab title `ONE`; group names `New`,
     `In progress`, `Pending` and `Resolved`; header `<n> to approve` and,
     on Pending, `longest wait <n> days`; heading `<n> new, <m> in progress`, or
     with none of either `Nothing needs you`; with no items
     at all, `Nothing here yet.`; a line `Environment: production`,
     `Environment: preview: <name>` or `Environment: development`.

2. **Opening a row, and nothing jumps.** Tapping a closed row opens it in
   place; tapping its top line closes it. Several rows can be open, and the
   page remembers on that device which ones. Opening a row in New makes the
   item In progress, but the row stays where it is while it is open and only
   appears at the top of In progress once it is closed. A row moves only when
   a new message from someone else arrives. Anything the owner, Claude or another
   agent does on a row leaves it where it is. While the page is open, items
   that arrive or come back take their place only after a reload or after
   The owner presses the button that says new items are waiting. On a phone the
   groups are headers and a tap opens a group. Keys work on a laptop: `j` and
   `k` to move, `Enter` to open, `Esc` to close, `c` for the note box, `a`
   for the agenda and `/` for search.
   - Exact wording: the button reads `1 new item` or `<n> new items`.

3. **Pending, Resolved and Closed.** The owner can mark an open item Pending
   (waiting for someone else) and Resolved. A Pending row shows for how long
   it has waited. When the owner's own reply goes out on an item that is In
   progress, the item moves to Pending. A Resolved item that has had no new
   message for 14 days (a setting, point 28) is closed by the app itself,
   with a notice. A new message from someone else brings a Pending, Resolved
   or Closed item back to New. Nothing else moves an item by itself. Agreed
   with the owner. Pending has no time limit (the owner): an item stays Pending until
   The owner moves it, a new message from someone else arrives in its thread, or
   Claude finds something new about it. For the third, Claude (the app's
   model, the night check, or an agent through the connector) can bring a
   Pending item back to New with one sentence saying what it found; this
   needs no Approve, because it only puts the item in front of the owner again.
   The time on a Pending row says how long the item has waited so far, and
   its words must not read as a limit.
   - Exact wording: buttons `Mark pending`, `Resolve` and `Reopen`; a
     pending row reads `waiting <n> days`, or `waiting <n> hours` under a
     day (`waiting 1 day`, `waiting 1 hour`); on an item Claude brought back
     `Claude: <reason>`; notices `You marked this pending.`, `You resolved this.`,
     `You reopened this.` and `Closed after 14 days without news.`

4. **What counts as a new message.** A mail whose read state, label, star or
   folder changed shows no new message and brings nothing back. A reaction on
   a chat message adds no message and brings nothing back. A chat message
   whose text was edited shows the new text marked as edited and brings the
   item back to Claude. An event that another person answered is updated and
   brings nothing back; an event whose time, place or text changed is
   updated and brings back its thread, if it has one. A message not seen
   before from someone else is added and brings the item back. A message
   sent by the owner himself, from anywhere, is added, never brings the item to
   Claude and, on an item In progress, moves it to Pending. Fetching the
   same unchanged message again changes nothing and writes nothing. Agreed
   with the owner.
   - Exact wording: an edited chat message is marked `edited`.

5. **Rows in three parts.** An open row has three parts side by side: who
   (a coloured chip with the item's number, the sender's own picture where
   there is one, the source's picture or two-letter badge (point 23), who it
   is from, when, and how many messages the conversation has), what (the subject, a line with the sender and time, the
   original text of the message as it was received, and the attachments), and
   the thread (point 6). The text is the sender's own words, never a summary;
   a message is shown in full when the row opens, because the owner must be able
   to read what was written before he reads what Claude says about it
  ; only a very long text (more than about forty lines) is
   folded and opens on request. The same person always gets the
   same colour. A chat that is still being written in is marked live. On a
   phone the three parts stack and the open thread fills the screen with the
   note box fixed at the bottom. On a phone everything that can be done
   with the open row is in that one bar at the bottom, as icons (the owner, looking at `Attach` and `Send` as words at the bottom and
   `Mark pending`, `Resolve` and `To do` as words behind the three dots:
   "vsetky ako attach/send mali byt ikony a to vratane obsahu za tromi
   bodkami, takto to je na dvoch miestach a dva rozne druhy ui zbytocne"):
   the row's own actions (mark pending, resolve or reopen, make a to-do),
   the note box, attach and send, each an icon with its word as the name a
   screen reader says; the icons are the ones the laptop's ribbon uses. No
   row on a phone has a three-dots menu. A closed row on a phone is resolved
   with one tap (the owner): it carries the resolve
   icon itself (in Resolved, the reopen icon); the row's other actions are
   in the bar once the row is open. After the tap a line at the bottom of
   the screen says what was done, with `Undo`, for a few seconds. A closed row is one compact line with the
   item's number, who it is from, the source, the subject, the beginning of
   the newest text and when. The buttons that move a row on (point 3) are
   reachable on the closed line too. Kept from an earlier version, with
   the stages instead of its pills.
   - Exact wording: folded text opens with `Show all` and closes with
     `Show less`; after a one-tap resolve on a phone `Resolved <number>.` with
     `Undo` (after a reopen `Reopened <number>.`); item numbers like `Q-0001`; a live chat is marked `Live`;
     sources as `Gmail`, `Office mail`, `Messenger`, `WhatsApp`,
     `iMessage`, `SMS` and `Calendar`.

6. **Every row is a thread.** The third part of a row is a chat about that
   item: entries from the owner, from Claude and from any other agent, each with
   who wrote it and when, oldest first, plus notices about what happened to
   the item. Under the thread there is always a box to write in. Pressing
   Enter sends what is written (the owner); Shift and Enter make a new line. A note from
   The owner is a request: the item is queued for Claude once, until Claude has
   answered or acted. Agents add their entries through the connector (point
   29). Notes from agents that only acknowledge something are folded, and
   notes next to each other fold into one line that opens in place; nothing
   is deleted by the app. The owner's own notes, proposals, results and notices
   are never folded.
   - Exact wording: The owner's entries are signed `you`, Claude's `Claude`,
     other agents' with the name they give; placeholder
     `Add a note or ask Claude…`; button `Send`; an empty entry is refused
     with `Write something first.`; with no entries yet, `No notes yet.`;
     the folded line reads `1 older note from <agent>` or
     `<n> older notes from <agent>`; with several agents, `<n> older notes`.

7. **Attachments in one place.** The owner can attach files to an entry he
   writes; agents can attach files to theirs. All attachments that belong to
   a row are kept in one list in the "what" part, under the original text:
   the files that came with any message of the conversation and the files
   attached in the thread, newest last, each with who added it. An entry that
   added a file says so and points to the list. Every attachment is a chip
   with its file name that downloads when tapped, only for someone signed in;
   files are never opened as pages. The owner can attach at most 5 files of at
   most 20 MB each at a time, as before; files that arrive with mail and chats
   have no limit. Kept from an earlier version.
   "Newest last" goes by when the file was sent or attached, not by when ONE
   saved it. Each file also shows that date; a file from a message older than
   the thread's window is still listed, with its date (the owner,
   after a contact card sent in 2024 and saved that day was taken for new).
   The date says which it is: sent for a file that came with a message,
   attached for one attached in the thread (the owner).
   - Exact wording: the list is headed `Attachments`; an entry that added
     files says `Attached: <file name>`; button `Attach`; the date is shown
     as in the thread, for example `26 Jan 2024`, after `sent` or `attached`:
     `added by <who> · sent <date>`, `added by <who> · attached <date>`.

8. **Message text and pictures.** Message text is shown as text: a subject
   or body that contains a script tag shows the tag itself and runs
   nothing. A mail in HTML is shown as text. A picture inside a mail or chat
   message (PNG, JPEG or GIF) is shown with the message, from the app's own
   copy. Opening a thread loads nothing from a sender's server, so no sender
   learns that it was read. The page loads fonts, icons and scripts only from
   the app's own address. Agreed with the owner.

9. **A thread shows the last month.** A thread shows the messages of the
   last month, newest first as in any conversation. Older messages are kept
   and searchable, and load with one button. This matters most for a chat
   with one person that runs for years. Agreed with the owner; the
   length of the window is a setting (point 28).
   - Exact wording: button `Show older`; the line above it reads
     `<n> older messages`.

10. **One matter, one row.** Messages of the same mail thread or chat sit
    under one row, newest first, each with its sender, time and subject.
    Messages about the same matter (a reference number such as an order,
    parcel, invoice or file number that matches a pattern the owner has set) join
    that row even when they arrive as separate mails. The app ships with no
    pattern; the owner adds them in settings, or Claude proposes one for his
    Approve. Similar sender or subject alone never joins rows. Claude can
    propose to merge two rows and to split one that was wrongly joined; both
    wait for Approve, and the number of a merged row leads to the row that
    stays. Nothing is lost. Kept from an earlier version.
    One matter can cross mail and chat (the owner, taken over from
    his work tool). A row can hold a mail thread and a chat
    about the same matter, joined the way two mail rows are joined:
    - Proposed, never by itself. Claude proposes the join with a reason and
      it takes effect only after the owner's Approve. A chat and a mail are
      never joined on their own, not even when they come from the same
      person, and a matter pattern never joins a chat to a mail.
    - After the join, new messages from either channel land in the one row,
      in time order, each marked with where it came from. A new message on
      any joined thread moves the row as a new message does today.
    - It can be undone: a joined chat or mail thread is split off again
      into its own row, also by proposal, and nothing is lost.
    - A reply on such a row answers the newest message, so it goes to that
      message's channel; the reply box says which channel and to whom,
      because the owner approves a send. The guards (Blocked targets, Same
      thread, Send once) are checked for the thread the reply would go to,
      not for the row as a whole.
    - A joined chat still waits until the conversation has gone quiet
      before Claude judges it (point 17), and on a phone a joined row
      counts as a thread that grew from a chat, so it is listed.
    - Joining rows does not join people; that stays its own proposal.
    A closed row whose thread holds a file shows a paperclip, on a laptop
    and on a phone, so the owner sees it without opening the row (the owner,
    taken over from his work tool); pictures inside a message's
    text and signature logos do not count. Its name for a screen reader is
    `Has attachments`.
    - Exact wording: notice in the row that stays
      `<agent> joined <number> into this row.`; settings section
      `Matter patterns`, button `Add pattern`.

11. **Claude looks first.** A new item waits for Claude before the owner sees it
    in the list; the page says how many are waiting. Claude then adds one
    summary to the thread, or a proposal, or nothing, and the item appears in
    New. An item that comes back through a new message is shown at once and
    goes to Claude again. A live chat is shown at once. A summary is an entry
    from Claude in the thread, never a replacement for the original text
    (the owner). There is
    one current summary per thread: a new one takes the place of the old one,
    and the old one stays in the record. A message of 300 characters or fewer
    gets no summary. Since a recent change the review starts by itself, inside
    the app: point 31 says how. Claude can still be connected from outside
    through the connector (point 29) and do the same things there.
    How Claude decides (the owner, after the first review of his real
    mail). A reason says why nothing is needed, not what kind of mail it is.
    A message that matters by its content (a deadline, a payment or one that
    failed, an expiry, a reminder, a change that touches money, anything
    from an office, a bank, a school, a tenant, an accountant, a customer) is
    never called noise, even when a machine sent it; when it is old and its
    date has passed, Claude says so and proposes to resolve it with a reason
    that begins `old:`. With every item Claude gets the notes that apply to
    it (by sender, person and matter) and follows them before its own
    judgement. A note that says a regular message is expected lets Claude
    skip only a message that matches the usual; when a message says there is
    a problem with a payment, or something has changed, it cannot be skipped:
    it is shown to the owner with what changed, and he decides. What the owner
    answers once in a thread  Claude
    saves as a note, so he is not asked again.
    - Exact wording: strip `<n> waiting for Claude`; a summary is marked
      `Summary`.

12. **Claude acts through proposals.** What Claude wants done (a reply, to
    mark pending or resolved, to merge or split, a to-do, a rule, a pattern,
    a guard, a person) is a proposal that does nothing until the owner presses
    `Approve`. On the closed row a chip in Claude's colour says what it
    proposes, with `Approve` and `Dismiss`, so one click works without
    opening the row. The chip also says why, in Claude's own words for
    that proposal, right there on the closed row (the owner, looking
    at four rows that each said only `Claude: Resolve` after he and Claude
    had gone through them together, so that he could not tell what he would
    be approving): the reason is read
    without opening the row, on a phone under the chip on up to two lines
    and in full when the row is opened; `Approve` and `Dismiss` stay
    together on one line. Only these take effect without Approve: comments,
    summaries, notes, and clear skips, where Claude closes noise or a chat
    that has finished. A clear skip always needs a reason, goes to Closed
    (point 27) and can be brought back. A proposal that no longer fits
    because a new message arrived or the owner acted himself is withdrawn by the
    app and shown as withdrawn. Dismissing is recorded. Agreed with the owner. Claude may also close or resolve a row when the owner's own
    newest note on it asks for it; the notice says so.
    - Exact wording: proposals are marked `Proposal`; buttons `Approve` and
      `Dismiss`; notice `You approved this proposal.`; a withdrawn proposal
      is marked `Withdrawn`; a result is marked `Result`; notice
      `<agent> closed this on your note.`

13. **Replies.** Agreed with the owner. A reply
    proposal shows the exact text in a box that the owner can edit, with where it
    goes, above `Approve` and `Discuss` (`Discuss` opens the note box so he
    can say what to change). `Approve` sends at once, from the app itself,
    exactly what the box shows at that moment, in the owner's name, into the same
    thread and to the people already on it: a Gmail reply, a Office mail
    reply, or a chat reply through the bridge. Both the original and the
    edited text are kept. The thread then says when it was sent or why it
    could not be (the item then needs the owner again, and his phone gets a
    notice). A chat reply goes out when the bridge next asks; while the Mac
    is off it waits, the thread says so, and the owner can withdraw it. On a
    phone `Approve` on a reply asks once more. Pressing `Approve` twice
    sends once, and a replaced or already sent proposal is never sent again.
    No reply is ever proposed on an item from a bank, a payment service, the
    data box (datová schránka) or a sender of sign-in codes; Claude's advice
    on those carries no text to send.
    - Exact wording: the box's label `Will be sent` followed by where, for
      example `Will be sent · reply in Messenger`,
      `Will be sent · reply to the mail` or
      `Will be sent · reply to the Office mail`; results `Sent <time>.`
      and `Could not send: <reason>`; while the Mac is off
      `Waiting for the Mac to send this.` with the button `Withdraw`; on a
      phone `Send this reply?` with `Send` and `Cancel`; notification title
      `Could not send`.

14. **Who gets in, and who is the owner.** Everything except `/api/` is behind
    the lab sign-in, attachments included. The page works on an iPhone home
    screen and on a laptop, with no sideways scrolling, in light and dark
    mode. Approving, rejecting, editing a proposal's text and changing any
    setting work only on the page, signed in. Whatever arrives at the open
    address or the connector, and whatever name it gives itself (including
    `the owner`), is an agent: it cannot approve, reject, send or change
    settings, and is refused. Agreed with the owner.
    - Exact wording: refusal
      `{"ok": false, "error": "only the owner can do this"}`; a wrong or missing
      token `{"ok": false, "error": "wrong token"}`.

15. **The app fetches by itself.** Agreed with the owner. Every
    5 minutes the app reads his Gmail, his Office mail, and his Google,
    Outlook and iCloud calendars with its own sign-ins, with no agent
    involved. Chats come from a small program on his Mac (point 16). The old
    sweep no longer brings items in: ONE accepts no sweep. How exactly Gmail
    and Outlook are read is covered by the app's own tests, not by the
    browser. The top bar has a `Refresh` button that fetches everything now
    instead of waiting; it costs nothing, and pressing it again while a fetch
    runs does nothing new. Kept from an earlier version.
    An agent can ask for the same refresh through the connector (the owner): it starts
    one if none is running, never two at once, and answers what each
    source brought and which source failed. It changes nothing else.
    After a refresh the page says what happened, not only that it ran
    (the owner, taken over from his work tool): how many new
    messages came, and each source that failed, by name, with the error in
    plain words, so that "nothing new" cannot be mistaken for a source
    that did not answer.
    - Exact wording: button `Refresh`; while running `Refreshing…`; after it
      finished, until the next reload, `Refreshed <time>`, followed by
      `<n> new` or `nothing new`, and for each failed source
      `<source> failed: <error>`.

16. **The chat bridge, and how tests put items in.** Chats (Messenger,
    WhatsApp, iMessage and SMS) arrive from the bridge, a small program on
    The owner's Mac next to Beeper. It connects out to the app, posts new chat
    messages and files to the app's open address with its own token, fetches
    approved chat replies to send, and reports the state of each network. The
    token is made by the app the first time it runs and is not the agents'
    token; it can do nothing else. A message posted this way becomes a chat
    item exactly like one from the real bridge, and the same message posted
    twice makes one. Tests create their own chat items this way with a unique
    marker in the text and remove only those. Removing works like this
    (the owner: the tests' data must be deletable before they run
    against the preview, because production gets that preview's data): a
    test names its run when it creates something, in the batch and in every
    agent call it makes for that run; the app marks what was created under
    that name, and whatever is then attached to a marked item (entries,
    proposals, to-dos, notes, files, the people seen only in marked chats)
    counts with it. One address removes everything of one run completely and
    says how many records of each kind it removed. It can never remove
    anything that does not carry a run's mark: not real mail, chats, people,
    notes, rules or settings, whatever it is asked. The real bridge never
    names a run. Reading Gmail, Outlook and the
    calendars cannot be driven from a browser test and is covered by the app's
    own tests. The exact addresses and fields under `/api/` are written down
    in the app's `API.md`, as before, and the test writer reads it
    together with this spec.
    - Exact wording: Connections page sections `Agent token` and
      `Bridge token`, each with the buttons `Copy` and `New token`.

17. **Chats are judged once they have finished talking.** Agreed with the owner
    and kept. A chat message is kept
    the moment it arrives and shown at once as live. The chat goes to Claude
    only once nobody has written in it for 30 minutes (a setting), and then
    once for the whole exchange. Claude judges it as finished (closed with a
    reason, listed under Closed), waiting for the owner, or waiting for someone
    else (Pending). A later message that needs the owner brings a closed chat back
    to New. Mail does not wait this way.
    The owner also answers in a chat himself, directly, attachments included
   . A
    chat row has, under its messages, a box to write a message into that
    chat. What the owner writes there is his own: it needs no `Approve`, Enter
    sends it (Shift and Enter make a new line), and it goes out through the
    bridge into the same chat, once. He can attach files with `Attach`
    (point 7's limit of 5 files of 20 MB each); each file goes as its own
    message after the text. His message shows in the chat at once, marked as
    going out, then as sent or as failed with the reason; while the Mac has
    not been heard from it waits and he can withdraw it. A message he writes
    never makes the chat wait for Claude. This box is only on chats; mail is
    answered through reply proposals (point 13). Nothing an agent does can
    put text into this box or send from it.
    The box is on a laptop only (the owner): on a phone a chat row shows
    its messages and the note box for Claude, and no box to write into the
    chat.
    And a phone does not list chats at all, unless a thread has grown from
    one (asked whether chats should still be listed on a phone, the owner
    said: not at all, unless a thread grows from one): a chat row is shown on a phone only
    when its thread holds something besides the chat's own messages: a
    comment or summary from Claude, a proposal, a note of the owner's or a
    to-do made from it. The phone's counts count what the phone lists. The
    laptop lists every chat as before.
    - Exact wording: notice on a chat that arrived finished `Arrived settled.`;
      the box's placeholder `Write in <network>…`, for example
      `Write in Messenger…`; button `Send`; button `Attach`; while going out
      `Sending…`; while the Mac is off `Waiting for the Mac to send this.`
      with `Withdraw`; on failure `Could not send: <reason>`.

18. **Agenda in a sidebar that hides.** Calendar entries of the three
    calendars (Google, Outlook for Office, iCloud) sit in a sidebar
    beside the list, one month back and one month ahead (settings, point 28).
    Every event of the selected calendars shows, not only accepted ones.
    Nothing clears by itself: past entries stay until someone ticks them off.
    Entries outside the window stay stored and are reached with one button.
    A button in the top bar hides and shows it; the page remembers the choice
    on that device. On a phone the agenda is a view of its own. Each event
    shows its calendar and, where known, whose it is (Klara's shared
    "Shared Calendar" calendar comes in through iCloud). Two filters, by person
    and by calendar, list only what the entries contain, and the page
    remembers the choice. Kept from an earlier version. The agenda
    shows where the owner is in time: a line across it at the
    present moment with the time, between the entries before and after, and
    it opens scrolled to that line; a button that brings him back to it
    stays visible while he scrolls.
    - Exact wording: button `Agenda`; the sidebar is titled `Agenda`; day
      headings start with `Today` and `Tomorrow`, other days show the date;
      with no events, `No events.`; filters `Person` and `Calendar`, each
      with the first choice `All`; entries with no person are under `Me`;
      when the filters hide everything, `No events match.`; button
      `Show earlier`; the line at the present moment reads `Now <time>`,
      for example `Now 21:30`; button `Now`.
    An entry says how the owner answered it, where the calendar knows (the owner,
    taken over from his work tool): an entry he declined, or
    accepted only tentatively, is marked, as a cancelled one already is, so
    that a declined meeting does not read as a commitment.
    - Exact wording: marks `Declined`, `Tentative` and `Cancelled`.

19. **To-dos.** Asked for by the owner. To-dos are
    listed in the agenda under the events, oldest first. The owner adds one by
    hand in a one-line box (an empty box adds nothing), or makes one from a
    row with a button; Claude and other agents can propose one, which is added
    after his Approve. A to-do made from a row shows the row's number and
    opens it. Tapping `Done` ticks it off and it leaves the section. A
    to-do is separate from its item: closing the item does not tick the
    to-do, and the reverse.
    On a phone the to-dos are not hidden behind the agenda and under a
    month of events (the owner): on the list
    page a group `To do` with its count stands with the stages, folded like
    them, and opens to the to-dos with `Done` and the box to add one; the
    count in the top line opens it. In the phone's agenda the to-dos come
    first, above the events.
    The to-dos are also a work list for agents (the owner agreed
    to take this over from his work tool): an agent can propose a to-do
    that belongs to no item, with an optional time or due date, and can
    propose to tidy the list: correct a to-do's words or its time, split
    one into several, join duplicates, tick one off or drop it. Every such
    proposal shows in the agenda with its reason, `Approve` and `Dismiss`,
    and changes nothing before the owner's Approve. These proposals change
    to-dos only; no agent can add, move or remove a calendar entry.
    - Exact wording: box label `Add a to-do`, button `Add`; button on a row
      `To do`; section heading `To do`; with none, `Nothing to do.`; button
      `Done`; notice in the thread `You made this a to-do.`

20. **iCloud calendars, read by the app itself.** Agreed with the owner and kept as is. The app reads his iCloud calendars over the
    internet (CalDAV) on the regular timer and on Refresh, and shows them in
    the agenda; calendars shared with him come with it. The owner connects it on
    the Connections page with his Apple ID and an app-specific password, kept
    with the app's secrets, never shown again, never in logs or through
    `/api/`, and not copied into previews. He can disconnect, which removes
    the password and the events it brought. Each calendar found is listed:
    The owner chooses whether it shows in the agenda and, if it is someone else's,
    whose it is (a person from People). Birthday, holiday and suggestion
    calendars start switched off. The app only reads; it never creates,
    changes or deletes anything in a calendar.
    - Exact wording: section heading `iCloud Calendar`; fields `Apple ID` and
      `App-specific password`; buttons `Connect` and `Disconnect`; when
      connected, `Connected as <Apple ID>`; per calendar a checkbox
      `Show in agenda` and a choice `Whose` with `Me` first; a wrong password
      is reported as
      `Apple refused the sign-in. Check the Apple ID and the app-specific password.`

21. **Import rules.** Agreed with the owner. Settings has a list of
    rules that decide what is not imported at all. A rule has a source, a
    field (sender, sender domain, subject, label or folder, network, chat
    type, muted, calendar), an operator and a value; the first enabled rule
    that matches wins. The owner adds, edits, switches and tests rules; a test
    shows what the rule would have dropped in the last 30 days. Each rule
    shows what it dropped: a list with sender, subject, time and the rule,
    and a button that brings one back by importing it again. The app starts
    with two rules: automatic GitHub pipeline mails, and muted group chats
    (so muted chats stay hidden and are counted in that list). Claude can
    only propose a rule; it applies after the owner approves it.
    Rules come from what the owner has done with his mail so far (the owner). The app keeps, for the whole history of each mailbox and not only
    the imported year, who wrote, when, and what the owner did with it: read or
    not, answered or not, starred, kept or deleted. For the years before the
    imported one it keeps only that, not the texts. From this Claude proposes
    two kinds of things, each with the numbers behind it (for example
    "214 mails in three years, none opened, none answered"): a rule that
    drops a kind of mail at import, and a note for its own review, such as
    "always show mail from this person" or "expected every month, skip
    unless something changed". Nothing applies until the owner approves it. A
    rule is proposed only for mail a machine sends in bulk that he has never
    opened or answered; anything he has ever answered, and anything that
    matters by its content (point 11), gets a note, never a rule.
    - Exact wording: settings section `Import rules`; buttons `Add rule`,
      `Test rule` and `Import again`; switch `Enabled`; each rule shows
      `Dropped (<n>)`; starting rules `GitHub pipeline mails` and
      `Muted group chats`; a proposed rule is marked `Proposal` with
      `Approve` and `Dismiss`.

22. **Guards.** Agreed with the owner. Settings lists the checks the
    app makes before it accepts an action, each with a plain explanation, its
    state and how often it refused something recently. Reason required, Protected
    people and Blocked targets can be switched off. The others cannot: Approval
    only, Exact text, Same thread, Send once, Approval required, and Actor by
    channel. Blocked targets holds the list of senders and domains (banks,
    payment services, the data box, sign-in code senders). Protected people
    means an item with a direct message from a protected person cannot be
    closed by an agent, only commented. An action a guard blocks is refused
    with a message that names the guard. Claude can only propose a change to a
    guard, which applies after the owner approves it.
    Claude can propose a change to anything in Settings, not only to guards
    (the owner): the value of a setting, a guard
    that can be switched, an entry on the Blocked targets list, an import
    rule, a skip rule, a matter pattern. Each such proposal shows what it
    changes, from what to what, and why, with `Approve` and `Dismiss`;
    nothing changes before the owner's Approve, so he can run the app without
    opening the Settings page. A guard that is always on cannot be
    proposed off, and no agent can change a setting itself.
    - Exact wording: settings section `Guards`; guard names `Reason required`,
      `Protected people`, `Approval only`, `Exact text`, `Same thread`,
      `Blocked targets`, `Send once`, `Approval required` and
      `Actor by channel`; state `On`, `Off` or `Always on`; `Refused recently:
      <n>`; refusal `Refused by guard: <guard name>`, followed by one
      sentence saying why (the owner: a refusal says why, so the
      agent changes its advice and the owner can read the reason in the
      record).

23. **Connections: a dot and a page of their own.** Next to the app's name in
    the top bar there is a status dot with the label below. Hovering shows
    each connection with its state; clicking opens the Connections page. For
    each connection the page shows its state, when it was last read, how many
    items it brought in the last 24 hours, its latest error with the time, a
    way to sign in or test it now where that applies, and its known
    limitations in plain words. A limitation is not an error and does not turn
    the dot. Connections: Gmail, Office mail, Chat bridge (with each
    network), Google Calendar, Outlook Calendar, iCloud Calendar (point 20),
    Notify and Claude (when an agent last called). The owner can set his own
    picture for each source and remove it again; a source without one shows a
    coloured two-letter badge. A stale source can be removed from the page and
    comes back when it is read again. The page also shows the recent fetches
    and the items per day and source for the last 14 days. Kept from an earlier version.
    Choosing a connection in the list's `Connections` pane shows only the
    rows that came through it, in every stage, until the owner shows all again
    (the owner, taken over from his work tool); the counts
    of the stages follow, and the choice is not kept after the page is
    left. On a phone the same works from the Connectors page.
    - Exact wording: above the list `Only <connection>` with the button
      `Show all`.
    - Exact wording: the top bar shows the name `ONE`; the dot's label is
      `Connectors: all fine`, `Connectors: needs a look` or
      `Connectors: a source has stopped`; page title and heading
      `Connectors`; statuses `OK`, `Stale`, `Error`, `Needs login` and
      `Not configured`; `Last read: <time>`; sections `Recent fetches`,
      `Last 14 days` and `Known limitations`; buttons `Sign in`, `Test now`,
      `Remove`, `Set picture` and `Remove picture`; a link
      `Back to the queue`; a picture that is not an image or over 300 KB is
      refused with `Use a PNG, JPEG or WebP picture under 300 KB.`;
      limitations `Chats arrive only while the Mac is on and Beeper is
      running.`, `A network that has logged itself out delivers nothing until
      The owner logs in on the Mac.`, `Phone calls are not covered.`, `Mail
      deleted before the first import cannot be imported.` and `Only the
      notification mails are seen. ONE never signs in to the data box, and a
      notification is not the delivery itself.`

24. **A failed source is never silent.** Agreed with the owner.
    After the third failed fetch in a row a source turns red, with the error,
    and the owner's phone gets one notice, not one per attempt. A sign-in that has
    run out pauses that source, shows `Needs login`, sends one notice, and
    fetching resumes by itself after the sign-in. A bridge silent for 15
    minutes (a setting) is a failed source and is named in the notice. A
    strip under the header names a failed source or one that needs login.
    Previews never notify. These need failing sources and cannot be driven by
    a browser test; the app's own tests cover them, and a browser test can
    only check what the Connections page and the strip say.
    - Exact wording: notification titles `<source> has stopped`,
      `<source> needs login` and `The Mac bridge is silent`; strip
      `<source> has stopped. See Connectors.`

25. **People, kept by Claude.** Agreed with the owner ; changed: The owner does not want to manage
    people himself , so Claude keeps
    them and the owner only tells it things, such as "always show me Ondra" or
    "this is my accountant". Every sender and chat participant is added on
    first sight, with the picture from the network. For each person the app
    keeps the name, what the owner calls them, who they are to him, and their
    addresses, numbers and handles on each source. Every message from a
    person's handle shows the name the owner calls them, one colour and their
    picture.
    Claude does by itself what only adds: what the owner calls a person, who
    they are to him, and switching `Always show me` on when the owner says so in
    a note or a chat. A person marked `Always show me` is protected (point
    22). Two things need the owner's `Approve` as a proposal: joining two people
    or two handles into one person (Lenka on WhatsApp and Lenka by mail), and
    switching `Always show me` off, so that nothing written in a message can
    take the protection away.
    The People page is no longer in the top bar. It is reached from Settings
    and is an overview: The owner sees what is kept about each person and can
    correct a mistake there, but he is not expected to work in it. It lists
    names only; a person opens when clicked. Handing a row over to someone
    stays switched off in this version.
    - Exact wording: link `People` on the Settings page; page title and
      heading `People`; field labels `Name`, `I call them`,
      `Who they are to me` and `Addresses and handles`; checkbox
      `Always show me`; buttons `Save`, `Set picture` and `Remove picture`;
      proposal cards `Join <name> and <name>` and
      `Stop always showing <name>` with `Approve` and `Dismiss`; with nobody
      yet, `No people yet.`; a link `Back to the queue`.

26. **Search and notes.** The top bar has a search over mail, chats, events,
    comments and notes, with the person as a filter; each result says what it
    is and opens its row. Notes are what the owner has explained once: a
    preference, a fact about a person, a decision, a way something is done.
    The owner and agents save and change them, on a page of their own, and attach
    one to a person or an item or leave it standing alone. Agents can look
    them up through the connector (point 29), together with the related
    threads, summaries and the person, in one answer.
    - Exact wording: search box placeholder
      `Search mail, chats, events and notes`; results marked `Mail`, `Chat`,
      `Event`, `Comment` and `Note`; with none, `No results.`; top bar link
      `Notes`; page title and heading `Notes`; button `Add note`; fields
      `Title` and `Note`; button `Save`; with none, `No saved notes yet.`

27. **Closed.** Asked for by the owner and kept: closed things leave
    the list and stay findable. A link under the list opens a page of all
    closed items, newest first, each with who closed it and why, and a button
    that brings it back to New with a notice. What Claude closed by itself
    (clear skips and finished chats) is listed first, as its own section. A
    new message in a closed item's conversation brings it back by itself
    (point 3). Nothing is deleted.
    Four eyes on what Claude closed (the owner: he wants the list
    of what Claude closed with a mark that he has seen it, and "zhrnutie
    jak mi pises tu raz za n doublecrosscheck4oci"). In `Closed by Claude`
    each entry the owner has not yet looked at is marked, with a button to mark
    it seen and one to mark all seen; the link under the list says how many
    are not seen. And the app keeps one summary of what Claude closed and
    The owner has not yet reviewed. It does not arrive as a new row every day:
    it floats as one task for the owner to review, and from there the owner and
    Claude settle it together (the owner). So there is one row of its own kind, with a to-do that points
    at it, as long as anything Claude closed is not seen; it is rewritten
    when Claude has closed more, at most once a day, in the evening, and
    The owner's phone gets at most one notice a day about it. In its thread
    The owner can write to Claude about any item it lists, and Claude answers
    and proposes there as in any thread. Marking the row resolved, or
    ticking its to-do, marks everything it lists as seen; the next thing
    Claude closes starts a new one.
    The summary must make sense (the owner):
    plain words, short, no filler; grouped by why things were closed, each
    group saying the reason once; the ones Claude was least sure of first,
    each with why it might matter after all; every item with who it is
    from, what it is and its number, so that one tap opens it or brings it
    back. It is written by the app's own model within the daily limit;
    without a key or over the limit it is a plain list grouped the same
    way.
    - Exact wording: marked entry `Not seen`; buttons `Seen` and
      `Mark all seen`; link `Closed (<n>, <m> not seen)`; the summary row's
      subject `Closed by Claude: <n> to review`; its to-do
      `Review what Claude closed`; setting `Summary of what Claude closed`
      with `On` and `Off`.
    - Exact wording: link `Closed (<n>)`, not shown when there are none; page
      title and heading `Closed`; sections `Closed by Claude` and
      `Everything else`; each entry `Closed by <who>` and `Reason: <reason>`;
      button `Bring back`; notice `You brought this back.`; a link
      `Back to the queue`.

28. **Settings, and an app that starts empty.** The top bar has a Settings
    page with the settings below, with these defaults, agreed with the owner. ONE starts empty. The first import goes back one year, newest
    first, and can be interrupted and resumed without writing anything twice.
    Items whose last message is older than the review window arrive as
    history: kept and searchable, but not waiting for Claude. A new message on
    such an item brings it to New.
    - Exact wording: page title and heading `Settings`; `Sync interval`
      `5 minutes`; `First import depth` `1 year`; `Review window` `1 month`
      (also the window of a thread); `Chat quiet time` `30 minutes`;
      `Bridge silence before a notice` `15 minutes`; `Agenda window back`
      `1 month`; `Agenda window ahead` `1 month`; `Quiet time before closing`
      `14 days`; section `Proposals from Claude` listing what waits for
      Approve; button `Save`.

29. **A connector for agents.** Kept from an earlier version. Everything
    agents can do is offered as an MCP server at `/api/mcp`, with the agents'
    token sent as `Authorization: Bearer <token>`, so Claude can be connected
    to ONE as a custom connector. The greeting and the list of tools are
    answered without a token and hold nothing of the owner's; every use of a tool
    needs the token. Each tool is named after what it does and marked as only
    reading or as changing something. Agents can: read what waits for review,
    read one item with its thread, comment and summarise (with files), propose
    a reply, mark an item seen, clear-skip an item with a reason, pull one
    back, propose a merge, a to-do, a change of an item, a rule, a pattern, a
    guard or a person, read the agenda and the settings, search, ask for
    background on a person or matter, save a note, record the owner's feedback on
    how an item was handled, list recent decisions and read the state of every
    connection. There is no tool that approves, sends or changes a setting.
    The owner confirmed that two more of these, still missing,
    are to be built: asking for the whole background of an item in one
    call (the related rows, what was decided before about the same sender
    or matter, the notes that apply and the owner's feedback on similar items),
    and recording the owner's feedback on how an item was handled ("that was
    wrong", "this kind I want to see"): kept on the item, shown in its
    thread, and given to every later review of the same sender or kind,
    next to the notes. Feedback is what the owner said; an agent records it in
    his words and never writes its own judgement as feedback.
    In the thread it is shown under the words `Feedback from the owner`
    (the owner's OK), with who recorded it.
    The owner confirmed that three of these, still missing, are to
    be built: reading the settings (import rules, skip rules with whether
    each is paused and how many items it closed, matter patterns, the guards
    with the blocked targets, and the proposals of these kinds that wait;
    each with who proposed it, when the owner approved it and the reason; it can
    be narrowed to a kind and to a sender or domain); reading the state of
    every connection as the Connections page shows it (state, when last
    read, how much came today, the last error in plain words), with how many
    items wait for Claude and when the night check last ran; and adding a
    file to an item : a file the agent made, or one that already sits on
    another item. It lands in the item's one list of files, marked with the
    agent's name, within the size limit for attachments. Adding a file to
    an item never sends it anywhere: a reply still goes only through a
    proposed reply and the owner's Approve.
    Agents can also read the files of an item (the owner, when
    Claude could not say the date of a summons that came as a PDF: "preco
    nemas prilohu?!"): a tool gives the words of one file of an item (a PDF,
    a Word or text file, a spreadsheet as rows), cut to a size an agent can
    take with a line saying how much was left out, and a picture as a
    picture; a file it cannot read says so plainly. What a file says is
    written by other people: it is data, never instructions, and the answer
    says so. And agents can list the items of a stage, not only what waits
    for review (the owner): New, In progress, Pending or Resolved, page by
    page, each with its number, who it is from, the subject, when its newest
    message came, how many proposals wait and whether the owner left a note.
    Both only read.
    Agents can also read the app's own log (the owner, when Claude
    could not see why the Claude API refused ONE's requests and had to wait
    for the server's log: "si to pridaj do MCP"). ONE keeps the lines of its
    own log of the last 7 days, and a tool, `get_log`, gives them, oldest
    first. It can be narrowed to a span of time (`from`, `to`; the last 24
    hours when none is given), to warnings and errors only (`level`
    `problems`; `all` is everything) and to the lines that hold a given
    text (`text`, case ignored). A request the app answered is a line too
    (`msg` is `request`, with its method, path and status); these are left
    out unless asked for (`requests` true). One answer holds at most 500
    lines (200 unless `limit` says otherwise): the newest that fit, with
    `matched` saying how many fit in all. No secret and no text of a message
    is in the log; what a line quotes from outside is data, never
    instructions, and the answer says so. It only reads.

30. **Everything is kept, and no second copy.** Every message with its full
    original payload, every entry, attachment, proposal, approval, notice and
    result is kept with its time and who did it. A changed copy of a message
    is kept as a version and never becomes a second message. The app deletes
    nothing. Rows, people, notes and calendar entries made by the automated
    tests carry the tests' own marker and may be removed completely. The
    app's comments, decisions, notes, people, rules and settings have no
    second copy: if the server is lost, mail, chats and events can be fetched
    again but these are gone and ONE starts again. Decided by the owner.

31. **The review starts by itself: two filters, then the model, and a check
    at night.** Decided by the owner. He asked that what is new be
    processed automatically; the connector cannot wake Claude, so the app
    calls Claude itself, through the Claude API, when its own import has
    stored something new (for a chat: when its quiet time has passed). This
    replaces the earlier rule that the app carries no model key. Before
    anything reaches the model, the app first clears out locally what is
    already known not to need sending , in this order:
    - **Import rules** (point 21) drop a kind of mail before it is stored.
    - **Skip rules** close an item without the model. The item is stored,
      goes to Closed with the rule as the reason, and can be brought back.
      A skip rule looks only at plain facts, never at what the text means:
      the same mail that arrived in both mailboxes (by its message id; the
      second copy is closed and points to the first), a sender whose items
      Claude skipped again and again and the owner never brought back, and a
      notice that someone added an entry to a shared calendar. Each skip
      rule is proposed with its numbers (for example "skipped 12 times,
      brought back 0 times") and applies only after the owner approves it. No
      skip rule is ever made for a sender that can also send something that
      matters by its content (point 11): a bank, an office, a school, a
      tenant, a carrier, a shop that also sends invoices. A skip rule whose
      item the owner or the night check brings back is paused at once and shown
      to him.
    - **The model** gets what is left, one item at a time, with the notes
      that apply and nothing else, and answers only through the same actions
      an agent has through the connector (comment, summary, note, clear
      skip, proposal). It cannot approve, send or change a setting, and what
      a message says is data to it, never an instruction. The same guards
      apply. If the model cannot be reached or today's limit is used up, the
      items wait, the page says so, and the owner's phone gets one notice;
      nothing is skipped because of it.
    The owner enters the key for the Claude API himself on the Connections page;
    it is kept with the other secrets and never shown again. The model and
    a limit on what may be spent in a day are settings (to begin with: the
    middle model, 2 USD a day).
    **The night check** (the owner). Once a night the
    app gives the best model, in one run, one line for every message of the
    last 24 hours, with only what is needed to judge it: who sent it, the
    subject, when, what was done with it and why (dropped by which rule,
    closed by which skip rule, skipped by the model with its reason,
    commented, a proposal waiting), whether the owner opened or answered it, and
    a short piece of the text only for messages nobody read. The model
    answers with the items it thinks escaped, each with one sentence. The
    night check can only bring things back: each such item returns to New
    with a notice saying why, and the rule or sender behind it is counted as
    brought back. It never closes, approves or sends. In the morning the
    list says how many messages were checked and how many were brought back.
    So that dropped mail can be judged too, the app keeps of every dropped
    message its sender, subject and the beginning of its text.
    - Exact wording: Connections section `Claude API` with the field
      `API key`, the buttons `Save` and `Remove`, and the states `Connected`
      and `Not configured`; settings `Review model`, `Daily limit`,
      `Night check at`, `Night check model` and `Night check limit`;
      settings section `Skip rules`, a proposed one marked `Proposal` with
      `Approve` and `Dismiss`, a paused one marked `Paused`; on an item
      closed by a skip rule `Closed by a rule: <rule name>`; on a second
      copy `Same mail as <number>`; while the limit is used up the strip
      `Review paused: today's limit is reached`; while the model cannot be
      reached `Review paused: Claude cannot be reached`; notification titles
      `Review paused`; on an item the night check brought back
      `Night check: <reason>`; the morning line
      `Night check: <n> messages, <m> brought back`.

32. **Work tool sees ONE.** The owner's aim: his
    work tool, Work tool connects to ONE's
    connector, so that he sees his private items beside his work items and
    ideally opens only Work tool. The two stay separate apps with separate
    stores and are connected afterwards . Private things are processed
    only by ONE and its own Claude ; the work side's agent does not handle them. the owner,
    Work tool gets "a ready list which is important and current",
    summarising his private tasks and agenda, clearly private, so that it
    need not work out what is what , and he
    can show or hide it with one click. What ONE gives Work tool:
    - its own token, named `Work tool`, made and revoked on the Connections
      page, separate from the agents' token and the bridge's; it can only
      read, and only the list below. It cannot create an item, write an
      entry, propose, approve, send, change a person, a note, a rule or a
      setting, and it cannot read messages, files or search. Nothing from
      work can be put into ONE through it;
    - one ready list, what is important and current: The owner's open to-dos,
      the items that wait for him (proposals waiting for his `Approve`, and
      items Claude says need him) and his agenda for today and the next 7
      days. Each entry has a short title, who it is from or what it is, one
      line on why it matters and what the owner should do, its date, and its
      address in ONE, so Work tool can open it there for the owner to approve,
      write or resolve. The lines are written by ONE and its own Claude,
      never by the work side's agent. The right to approve and to send stays
      in ONE;
    - the list is marked as private and says when it was made. It carries no
      message texts and no files; to see more, the owner opens the entry in ONE;
    - Work tool shows it apart from work, as private, and the owner can show or
      hide it with one click (that part is Work tool's, not ONE's).
    Acting from Work tool as the owner (notes, resolving, approving) is a later
    step that the owner decides after the reading has run for a while; it needs
    its own credential that no agent on the work side can use.
    - Exact wording: Connections section `Work tool token` with the buttons
      `Copy`, `New token` and `Revoke`.

33. **A meeting can have a thread.** the owner, taken over from his
    work tool. Not every calendar entry gets a thread (every private entry
    is the owner's own and the list would flood); one is made only when someone
    asks for it: The owner opens a thread from an agenda entry, or Claude
    proposes one with a reason and the owner approves. The thread is a normal
    row of its own kind, tied to the calendar entry: notes, comments, files
    and proposals work as on any row. It follows the entry: a changed time
    or place, or a cancellation, shows in the thread as a notice and moves
    the row as a new message does. Related mail and chats can be joined to
    it by proposal (point 10). A repeating entry has one thread for the
    whole series. The entry stays in the agenda whatever happens to the
    thread; closing the thread does not remove it, and the app never
    changes the calendar.
    - Exact wording: button on an agenda entry `Open thread`, and once one
      exists `Thread`; notices in the thread `Moved to <day and time>.`,
      `Place changed to <place>.` and `The entry was cancelled.`

34. **History in batches.** the owner, taken over from his work tool
    (he wants the whole mail history gone through and rules made from it).
    It runs only on the owner's word , through
    two tools for agents: one sizes up and then hands out the next stretch
    of old threads, one records the result for a thread.
    - Batches go backward: the first starts where the review window ends
      (the last month is the live queue), each next one where the last
      stopped; the app remembers the point reached. They reach back as far
      as mail is stored in full (`First import depth`, a year unless the owner
      sets more); older than that the app holds only the numbers per
      sender, with no text to read (corrected with the owner's OK; the first wording said "where the imported year ends"). A named period also works. A batch can be limited to threads
      The owner took part in.
    - Size and cost first: before any thread is handed out, the tool says
      how many threads the period holds and roughly how much text, with an
      estimate of what the model would cost, so the owner can shorten it.
    - Per thread Claude records a short summary and how it ended: answered,
      ignored, handed on or still open.
    - Old threads stay out of New. The one exception is a thread Claude
      marks as needing attention (a question that never got an answer): it
      goes to New with Claude's comment.
    - Rules come out of it: where a sender is pure noise, Claude proposes
      an import rule or a skip rule with the numbers; where the owner's past
      handling shows a pattern, Claude saves a note on the sender. A
      message that matters by its content is never noise, in history as in
      new mail (point 11).
    - It can stop at any point; threads already done stay done.
    - Settings shows how far back the review has reached and how much is
      left.
    - Exact wording: settings section `History review` with the line
      `Reviewed back to <date>, <n> threads left`; before any batch
      `Not started`; on a thread brought to New
      `History: <reason>`.

## Not in this version

- Anything from the owner's employer (no work mail, Teams chat or case).
- Phone calls.
- A "Not now" button: an open item stays in its group.
- Handing a row over to a person: the `For` control stays hidden.
- New mails or chats to people who are not on the thread; replies only.
- Creating or moving calendar events.
- Payments, bank actions, the data box and any sign-in on the owner's behalf.
- Search by meaning (full-text search only).

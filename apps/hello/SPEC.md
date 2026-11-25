# Hello

A tiny notes app. Its job is to prove the whole pipeline works: building,
testing, previews with real data, production, rollback.

## What it must do

1. **Front page.** Opening the app shows a page titled "Hello" with the heading
   "Hello", and says which environment it runs in.
   - Exact wording: browser tab title `Hello`; heading `Hello`; a line
     `Environment: production`, `Environment: preview: <name>` or
     `Environment: development`.

2. **Add a note.** There is a text field for a note and an "Add" button. Typing
   a note and pressing "Add", or pressing Enter in the field, puts the note at
   the top of the notes list and empties the field.
   - Exact wording: field label `Note`; button `Add`; the notes are a list
     named `Notes`.

3. **Notes are kept.** A note is still there after reloading the page.

4. **Newest first.** The newest note is always at the top of the list.

5. **Empty notes are refused.** Pressing "Add" with an empty field, or one with
   only spaces, adds nothing and shows a message.
   - Exact wording: `Write something first.`

6. **Notes are short.** A note holds at most 280 characters; the field does not
   accept more.

7. **Delete a note.** Every note has a "Delete" button. Pressing it removes that
   note, and it stays removed after reloading.
   - Exact wording: button `Delete`.

8. **Phone and desktop.** The page fits a phone screen without sideways
   scrolling, and everything above works the same on phone and desktop, in the
   Chrome, Firefox and Safari engines.

9. **Home screen.** The app can be added to an iPhone home screen with the name
   "Hello" and its own icon.
   - Exact wording: web app name `Hello`.

10. **Version.** The bottom of the page shows which version is running: the
    first 7 characters of the commit, or `dev` when run locally.
    - Exact wording: `Version: <version>`.

11. **Health.** The address `/healthz` answers `ok`.

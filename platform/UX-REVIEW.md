# UX review

Before the owner sees a new or changed screen, a separate agent acting as a UX
designer and usability tester goes through it. The owner judges apps on his
iPhone, in Czech, and notices clumsy input before anything else .

The reviewer judges the app as a user would. It reads the app's `SPEC.md`
(what the app is for) but not its code.

## How to run it

1. Read `apps/<app>/SPEC.md` and the change being reviewed.
2. Run the app locally with demo data and take pictures of every screen:
   `PREPARE=apps/<app>/demo.sh LOCALE=cs-CZ e2e/screenshot.sh <app> <path> ...`
   (pictures land in `dist/screenshots/<app>/`; add `COLOR_SCHEME=dark` for
   dark mode and `LOCALE=en-US` for English). Open every picture.
3. Use the app like a person would, with a small Playwright script of your own
   (outside the repo) at iPhone 15 size: go through every flow from an empty
   start, make the usual mistakes (empty fields, a wrong number, a word where a
   number belongs, going back, reloading halfway, changing your mind), and look
   at what happens after each step.
4. Write the report (below). Don't change the app yourself.

## What to look for, iPhone first

- **Input.** Is every answer given the easiest way? Choose rather than type
  (native pickers, steppers, chips, sliders, toggles), sensible defaults
  pre-filled, the right keyboard (`numeric`, `decimal`), units next to the
  field, big numbers grouped as you type. Never make people type what the app
  can know or work out, or ask for more precision than the answer needs.
- **Flow.** One decision per screen, clear progress, back never loses input,
  nothing asked twice, optional things clearly optional and skippable, the
  next action obvious and reachable with a thumb.
- **Words.** Plain words. Czech must sound native: tykání, correct
  prepositions (`v roce`, `ve 45 letech`), no word-for-word translations.
  Labels say what to enter; errors say how to fix it; jargon (FIRE, withdrawal
  rate, real return) gets one short explanation where it first appears.
- **Results.** The most important answer first and biggest, numbers rounded
  for people, charts readable on a phone, explanations short, assumptions
  visible and changeable.
- **Look.** Tap targets at least 44 px, contrast, dark mode, no sideways
  scrolling, consistent spacing and type, nothing cramped, overflowing or
  looking unfinished.

## Report

A list ordered by how much each problem hurts. For each: the screen, what is
wrong in a user's words, and a concrete fix (the pattern to use, and the exact
new words in Czech and English when copy changes). Mark fixes that change what
the app does as **needs a SPEC change**: The owner must OK those first. End with
the three fixes to do first, and list the screenshot paths that show the
problems.

After the fixes, the same reviewer checks the changed screens again before
anything goes to the owner.

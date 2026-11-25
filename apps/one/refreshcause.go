package main

import "strings"

// The causes a failed source is told by (SPEC 15, SPEC 24): one short
// fragment, chosen by cause. The run's own words, which can hold a server's
// text, stay in the log ("detail" on the run's end line); the page, the
// refresh notice and the tools never show an address, a name twice or a
// nested colon.
const (
	causeUnreachable = "could not reach the server"
	causeRefused     = "the sign-in was refused"
	causeRunOut      = "the sign-in has run out"
	causeServerError = "the server answered an error"
	causeSlow        = "it took too long"
	causeSilent      = "it did not answer"
	causeUnknown     = "it could not be read"
)

// plainCause maps what a failed run said to one fixed fragment. An empty
// text stays empty.
func plainCause(raw string) string {
	t := strings.ToLower(strings.TrimSpace(raw))
	if t == "" {
		return ""
	}
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(t, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("run out", "ran out", "expired", "invalid_grant", "needs login", "needs a new login", "not signed in", "sign in again"):
		return causeRunOut
	case has("connection refused", "connection reset", "no such host", "could not reach", "could not be reached", "unreachable", "dial tcp", "eof", "broken pipe", "tls"):
		return causeUnreachable
	case has("did not answer in time", "timed out", "timeout", "deadline", "too long", "slow down"):
		return causeSlow
	case has("not been heard from", "did not answer", "not answering"):
		return causeSilent
	case has("refused", "401", "403", "unauthorized", "unauthorised", "forbidden", "authentication", "password", "denied", "sign-in", "does not accept"):
		return causeRefused
	case has("500", "502", "503", "504", "status", "server error", "unexpected", "answered"):
		return causeServerError
	}
	return causeUnknown
}
